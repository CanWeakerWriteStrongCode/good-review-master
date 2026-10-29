package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"good-review-master/logutil"
	"good-review-master/telemetry"

	openai "github.com/sashabaranov/go-openai"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// DataURL 把图片字节编码成 data URL（视觉模型的 image_url 内联标准形态）。
func DataURL(mime string, data []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// 对话消息角色（中立定义，避免上层直接依赖 openai SDK 的常量）
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// ToolCall 模型发起的一次工具调用
type ToolCall struct {
	ID        string // 回填结果时必须原样带回，模型靠它对应哪次调用
	Name      string // 工具名
	Arguments string // 入参，JSON 字符串
}

// Message 一条对话消息。
// Role=assistant 且带 ToolCalls 表示模型要求调工具；
// Role=tool 表示某次调用的结果，ToolCallID 必须与发起时的 ID 一致。
// ImageDataURLs 非空时本消息改发多模态内容（MultiContent：文本 + image_url data URL），
// 供视觉模型直接看到图片；为空则保持纯文本 Content，字节与旧路径一致（保住前缀缓存）。
type Message struct {
	Role          string
	Content       string
	ImageDataURLs []string
	ToolCalls     []ToolCall
	ToolCallID    string
	Name          string
}

// Tool 注入给模型的工具定义。ParamsJSON 是原始 JSON Schema，用 RawMessage
// 透传到 OpenAI 的 function.parameters，避免被当成普通字符串二次转义。
type Tool struct {
	Name        string
	Description string
	ParamsJSON  json.RawMessage
}

// emptyParamsSchema 工具未给出入参 schema 时的兜底（OpenAI 要求 parameters 是合法 JSON Schema 对象）
var emptyParamsSchema = json.RawMessage(`{"type":"object","properties":{}}`)

// Image 一张图片字节（MCP 工具结果/内置「看图」返回），
// 上层用 llm.DataURL(mime, data) 转成 image_url 的 data URL 塞进 ImageDataURLs。
type Image struct {
	Data     []byte
	MIMEType string
}

// ChatResponse 一次对话的模型输出：要么给最终文本，要么要求调用工具
type ChatResponse struct {
	Content   string
	ToolCalls []ToolCall
	// FinishReason 模型这轮的结束原因：stop=正常作答 | tool_calls=要调工具 | length=超长被截断。
	// 诊断「模型为什么死活不调工具」时很关键：若工具已下发却一直是 stop，多半是中转没透传 tools。
	FinishReason string
	// PromptTokens / CompletionTokens 是服务端返回的用量（有些中转不返回，那时为 0）。
	// 带出来是为了让离线评测集（tests/eval）能报出真实花费，
	// 而不用本地估算去猜——本地估算只服务于选窗决策，口径不同。
	PromptTokens     int
	CompletionTokens int
}

// Client 大模型统一接口
type Client interface {
	// Review 单轮、不带工具的调用（内部指令生成提示词/规则用）
	SingleChat(ctx context.Context, chatLog, systemPrompt string) (string, error)
	// Chat 多轮对话；tools 非空时启用原生 function calling，
	// 返回的 ToolCalls 非空表示模型要求调工具而非已给出最终答案
	MutiChatWithTool(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error)
	// ChatJSON 单轮、无工具、约束输出为 JSON。
	// 优先用 response_format=json_schema(strict)，后端不支持时自动降级
	// json_object → 不传 response_format（纯提示词约束），档位会被记住。
	ChatJSON(ctx context.Context, messages []Message, schema json.RawMessage) (*ChatResponse, error)
}

// OpenAIAdapter 适配所有OpenAI协议的大模型（基于 go-openai SDK）
type OpenAIAdapter struct {
	client *openai.Client
	model  string
	temp   float32
	topP   float32

	// jsonMode 记录 ChatJSON 可用的 response_format 档位（0=schema 1=object 2=none）。
	// 后端对不支持的字段会回 400，降级一次后就不必每条消息都重试同一档。
	jsonMode atomic.Int32

	// noThinkingOff 记录后端是否拒绝 chat_template_kwargs（false=带，true=已剥离）。
	// 只有 ChatJSON（裁判）会带它：严格云端把未知字段当 400，剥离一次后不再发。
	noThinkingOff atomic.Bool
}

// noThinkingKwargs 关闭推理模型的思考阶段（Qwen3 系 chat template 认 enable_thinking）。
// 裁判只判断"回/不回"，实测本机 llama.cpp 带思考 14s / 842 tokens，
// 关掉后 1.6s / 63 tokens，判断结果不变。
var noThinkingKwargs = map[string]any{"enable_thinking": false}

// ChatJSON 的 response_format 档位。
const (
	jsonModeSchema int32 = iota
	jsonModeObject
	jsonModeNone
)

var jsonModeNames = [...]string{"json_schema", "json_object", "none"}

// NewOpenAIAdapter 创建OpenAI兼容的大模型客户端
func NewOpenAIAdapter(apiKey, apiBase, model string, temp, topP float64) Client {
	cfg := openai.DefaultConfig(apiKey)
	cfg.BaseURL = strings.TrimSuffix(apiBase, "/")
	return &OpenAIAdapter{
		client: openai.NewClientWithConfig(cfg),
		model:  model,
		temp:   float32(temp),
		topP:   float32(topP),
	}
}

// Review 单轮调用大模型（不带工具），等价于 Chat 的两条消息包装
func (adapter *OpenAIAdapter) SingleChat(ctx context.Context, chatLog, systemPrompt string) (string, error) {
	logutil.Info("发送给大模型", "systemPrompt", systemPrompt, "chatLog", chatLog)
	resp, err := adapter.MutiChatWithTool(ctx, []Message{
		{Role: RoleSystem, Content: systemPrompt},
		{Role: RoleUser, Content: chatLog},
	}, nil)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// Chat 多轮对话；tools 非空时下发原生 function calling 工具清单
func (adapter *OpenAIAdapter) MutiChatWithTool(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	return adapter.chatCompletion(ctx, messages, tools, nil, nil)
}

// ChatJSON 见 Client 接口。按 schema → json_object → 无 的档位逐级降级：
// 只有"看起来是 response_format 被拒"的 4xx 才降级，其它错误原样返回，
// 避免把超时/鉴权问题误当成能力缺失。
func (adapter *OpenAIAdapter) ChatJSON(ctx context.Context, messages []Message, schema json.RawMessage) (*ChatResponse, error) {
	for {
		mode := adapter.jsonMode.Load()
		var format *openai.ChatCompletionResponseFormat
		switch mode {
		case jsonModeSchema:
			format = &openai.ChatCompletionResponseFormat{
				Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
				JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
					Name:   "structured_output",
					Strict: true,
					Schema: schema,
				},
			}
		case jsonModeObject:
			format = &openai.ChatCompletionResponseFormat{
				Type: openai.ChatCompletionResponseFormatTypeJSONObject,
			}
		}

		var kwargs map[string]any
		if !adapter.noThinkingOff.Load() {
			kwargs = noThinkingKwargs
		}
		resp, err := adapter.chatCompletion(ctx, messages, nil, format, kwargs)
		if err == nil {
			telemetry.LLMResponseFormatTotal.WithLabelValues(jsonModeNames[mode]).Inc()
			return resp, nil
		}
		// 后端不认识 chat_template_kwargs：剥离后重试同一档。
		// 必须排在 response_format 降级之前，否则会把"字段不支持"误判成"格式不支持"。
		if !adapter.noThinkingOff.Load() && chatTemplateKwargsRejected(err) &&
			adapter.noThinkingOff.CompareAndSwap(false, true) {
			logutil.Warn("模型不支持 chat_template_kwargs，裁判将不再关闭思考",
				"model", adapter.model, "err", err)
			continue
		}
		if mode == jsonModeNone || !responseFormatRejected(err) {
			return nil, err
		}
		next := mode + 1
		if adapter.jsonMode.CompareAndSwap(mode, next) {
			logutil.Warn("模型不支持该 response_format，已降级",
				"model", adapter.model,
				"from", jsonModeNames[mode], "to", jsonModeNames[next], "err", err)
		}
	}
}

// chatCompletion 是两条调用路径的公共实现：组装请求、打日志、计时、埋点、解析响应。
// responseFormat 为 nil 时不带 response_format 字段；chatTemplateKwargs 为 nil 时
// 不带 chat_template_kwargs 字段（主回复路径保持与改造前逐字节一致）。
func (adapter *OpenAIAdapter) chatCompletion(ctx context.Context, messages []Message, tools []Tool,
	responseFormat *openai.ChatCompletionResponseFormat, chatTemplateKwargs map[string]any) (*ChatResponse, error) {
	formatName := "none"
	if responseFormat != nil {
		formatName = string(responseFormat.Type)
	}
	// 大模型调用是整条链路里最慢、最贵、最可能失败的一跳，span 与指标都埋在这里。
	// 放在适配器而不是调用方：SingleChat 也走这个函数，两处入口都能覆盖到。
	ctx, span := telemetry.StartSpan(ctx, "llm.chat",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("llm.model", adapter.model),
			attribute.Int("llm.tool_count", len(tools)),
			attribute.String("llm.response_format", formatName),
		))
	defer span.End()

	req := openai.ChatCompletionRequest{
		Model:              adapter.model,
		Messages:           toOpenAIMessages(messages),
		Temperature:        adapter.temp,
		TopP:               adapter.topP,
		Tools:              toOpenAITools(tools),
		ToolChoice:         "auto",
		ResponseFormat:     responseFormat,
		ChatTemplateKwargs: chatTemplateKwargs,
	}
	// Debug 级把「发给大模型的完整请求 JSON」整段打出来。
	// 字段顺序按模型 chat template 的渲染顺序排列（tools 在 messages 之前），
	// 与 go-openai 实际发送的请求语义一致、仅键顺序不同——避免日志看着像"工具定义塞在对话后面"。
	// Debug 默认开启；收紧到 info 及以上时这条不再输出，用于核对模型到底收到了什么。
	if raw, err := marshalRequestForLog(req); err == nil {
		logutil.Debug("发给大模型的完整请求JSON", "body", json.RawMessage(raw))
	} else {
		logutil.Debug("发给大模型的完整请求JSON序列化失败", "err", err)
	}

	callStart := time.Now()
	resp, err := adapter.client.CreateChatCompletion(ctx, req)
	elapsed := time.Since(callStart)

	// 成败两条路径都要记耗时：只统计成功请求会把"超时"这类最该看见的情况
	// 从耗时分布里抹掉，于是 P99 看起来永远很健康。
	telemetry.LLMDurationSeconds.WithLabelValues(adapter.model).Observe(elapsed.Seconds())
	if err != nil {
		telemetry.LLMRequestsTotal.WithLabelValues(adapter.model, telemetry.ResultError).Inc()
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, fmt.Errorf("大模型调用失败: %w", err)
	}
	telemetry.LLMRequestsTotal.WithLabelValues(adapter.model, telemetry.ResultOK).Inc()
	// token 用量取服务端返回的 usage，不是本地估算——本地估算（cache.EstimateTokens）
	// 只服务于选窗决策，两者口径不同，不要混用。
	// 有些中转不返回 usage（全 0），那种情况下这两条曲线会是平的，属于上游缺失而非统计错误。
	telemetry.LLMTokensTotal.WithLabelValues("prompt").Add(float64(resp.Usage.PromptTokens))
	telemetry.LLMTokensTotal.WithLabelValues("completion").Add(float64(resp.Usage.CompletionTokens))
	span.SetAttributes(
		attribute.Int("llm.usage.prompt_tokens", resp.Usage.PromptTokens),
		attribute.Int("llm.usage.completion_tokens", resp.Usage.CompletionTokens),
	)

	// Debug 级把模型返回的完整原始 JSON 整段打出，与上面的请求 JSON 对照：
	// 能直接看到 finish_reason 是 stop 还是 tool_calls、message.tool_calls 里有没有函数名。
	if raw, err := marshalForLog(resp); err == nil {
		logutil.Debug("大模型原始返回JSON", "body", string(raw))
	} else {
		logutil.Debug("大模型原始返回JSON序列化失败", "err", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("大模型返回为空")
	}
	choice := resp.Choices[0]
	msg := choice.Message
	out := &ChatResponse{
		Content:          msg.Content,
		FinishReason:     string(choice.FinishReason),
		PromptTokens:     resp.Usage.PromptTokens,
		CompletionTokens: resp.Usage.CompletionTokens,
	}
	for _, tc := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return out, nil
}

// responseFormatRejected 判断错误是否像"后端不认识 response_format"。
//
// 只看 400/404/422/501 这类参数错误：鉴权(401/403)、限流(429)、超时(5xx 其他)
// 与输出格式无关，降级只会把同一个错误再打两遍。
// 部分中转的报错很笼统（不写 response_format），但同一份 messages 在没有该字段时
// 是能通的，所以参数类状态码就够用了。
func responseFormatRejected(err error) bool {
	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return formatRejectStatus(apiErr.HTTPStatusCode)
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		return formatRejectStatus(reqErr.HTTPStatusCode)
	}
	return false
}

// chatTemplateKwargsRejected 判断错误是否像"后端不认识 chat_template_kwargs"。
//
// 只有报错里点名字段时才剥离：其余 4xx 交给 response_format 降级链，
// 免得每换一个后端都多打一趟无效请求。
func chatTemplateKwargsRejected(err error) bool {
	if !responseFormatRejected(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "chat_template_kwargs") || strings.Contains(msg, "enable_thinking")
}

func formatRejectStatus(status int) bool {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound,
		http.StatusUnprocessableEntity, http.StatusNotImplemented:
		return true
	}
	return false
}

// marshalRequestForLog 序列化调试日志用的请求体：tools 放在最前，messages 在后，
// 与模型 chat template 的渲染顺序一致；JSON 语义与实际发送的请求完全相同。
//
// 用内嵌原始请求而不是复制字段：encoding/json 遇到不同层级的同名字段时浅层胜出，
// 外层的 Tools 会覆盖内嵌请求里的 tools，不会出现重复键，其余字段仍跟随 go-openai 定义。
func marshalRequestForLog(req openai.ChatCompletionRequest) ([]byte, error) {
	ordered := struct {
		Tools []openai.Tool `json:"tools,omitempty"`
		openai.ChatCompletionRequest
	}{Tools: req.Tools, ChatCompletionRequest: req}
	return marshalForLog(ordered)
}

// marshalForLog 只用于调试日志：把 JSON 里的 HTML 转义还原成可读字符。
//
// 不能用 json.Marshal：它默认把 @ 标记的 <@openid> 写成 \u003c@openid\u003e。
// 只关 SetEscapeHTML 也不够——go-openai 的消息结构自带 MarshalJSON，内部仍会转义，
// 所以最后再把这三个转义序列换回来（纯日志用途，不影响实际请求）。
func marshalForLog(value any) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	out := bytes.TrimRight(buf.Bytes(), "\n")
	unescaped := unescapeHTMLForLog(out)
	// 替换若把 JSON 改坏（理论上不该发生），宁可打转义版，也不能让日志字段序列化失败。
	if json.Valid(unescaped) {
		out = unescaped
	}
	return out, nil
}

// unescapeHTMLForLog 还原 \u003c / \u003e / \u0026，但跳过本身已被转义的反斜杠。
//
// 消息正文里可能真的写着 "\u003c" 这六个字符，此时 JSON 中是 `\\u003c`：
// 直接做字节替换会得到非法的 `\<`，整个 RawMessage 就再也序列化不出来。
// 所以按反斜杠连续段判断：段长为奇数，最后一个反斜杠才开启真正的转义。
func unescapeHTMLForLog(out []byte) []byte {
	if !bytes.Contains(out, []byte(`\u00`)) {
		return out
	}
	var result bytes.Buffer
	result.Grow(len(out))
	for i := 0; i < len(out); {
		if out[i] != '\\' {
			result.WriteByte(out[i])
			i++
			continue
		}
		run := 0
		for i+run < len(out) && out[i+run] == '\\' {
			run++
		}
		if i+run+5 > len(out) || run%2 == 0 {
			result.Write(out[i : i+run])
			i += run
			continue
		}
		var replacement byte
		switch string(out[i+run : i+run+5]) {
		case "u003c":
			replacement = '<'
		case "u003e":
			replacement = '>'
		case "u0026":
			replacement = '&'
		}
		if replacement == 0 {
			result.Write(out[i : i+run])
			i += run
			continue
		}
		// 前 run-1 个反斜杠成对，代表字面量反斜杠，原样保留。
		result.Write(out[i : i+run-1])
		result.WriteByte(replacement)
		i += run + 5
	}
	return result.Bytes()
}

// toOpenAIMessages 把中立消息列表转成 go-openai 的消息结构
func toOpenAIMessages(messages []Message) []openai.ChatCompletionMessage {
	out := make([]openai.ChatCompletionMessage, 0, len(messages))
	for _, m := range messages {
		msg := openai.ChatCompletionMessage{
			Role:       m.Role,
			Content:    m.Content,
			Name:       m.Name,
			ToolCallID: m.ToolCallID,
		}
		// 带图消息改用 MultiContent（Content 与 MultiContent 互斥），
		// 文本在前、图片 data URL 在后；无图则保持纯文本 Content，字节与旧路径一致。
		if len(m.ImageDataURLs) > 0 {
			parts := make([]openai.ChatMessagePart, 0, len(m.ImageDataURLs)+1)
			if strings.TrimSpace(m.Content) != "" {
				parts = append(parts, openai.ChatMessagePart{Type: openai.ChatMessagePartTypeText, Text: m.Content})
			}
			for _, u := range m.ImageDataURLs {
				parts = append(parts, openai.ChatMessagePart{
					Type:     openai.ChatMessagePartTypeImageURL,
					ImageURL: &openai.ChatMessageImageURL{URL: u},
				})
			}
			msg.Content = ""
			msg.MultiContent = parts
		}
		for _, tc := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, openai.ToolCall{
				ID:   tc.ID,
				Type: openai.ToolTypeFunction,
				Function: openai.FunctionCall{
					Name:      tc.Name,
					Arguments: tc.Arguments,
				},
			})
		}
		out = append(out, msg)
	}
	return out
}

// toOpenAITools 把中立工具定义转成 go-openai 的 tools 字段
func toOpenAITools(tools []Tool) []openai.Tool {
	out := make([]openai.Tool, 0, len(tools))
	for _, t := range tools {
		params := t.ParamsJSON
		if len(params) == 0 {
			params = emptyParamsSchema
		}
		out = append(out, openai.Tool{
			Type: openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  params,
			},
		})
	}
	return out
}
