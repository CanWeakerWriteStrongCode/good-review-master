package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"good-review-master/logutil"

	openai "github.com/sashabaranov/go-openai"
)

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
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	Name       string
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

// ChatResponse 一次对话的模型输出：要么给最终文本，要么要求调用工具
type ChatResponse struct {
	Content   string
	ToolCalls []ToolCall
}

// Client 大模型统一接口
type Client interface {
	// Review 单轮、不带工具的调用（内部指令生成提示词/规则用）
	Review(ctx context.Context, chatLog, systemPrompt string) (string, error)
	// Chat 多轮对话；tools 非空时启用原生 function calling，
	// 返回的 ToolCalls 非空表示模型要求调工具而非已给出最终答案
	Chat(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error)
}

// OpenAIAdapter 适配所有OpenAI协议的大模型（基于 go-openai SDK）
type OpenAIAdapter struct {
	client *openai.Client
	model  string
	temp   float32
	topP   float32
}

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
func (adapter *OpenAIAdapter) Review(ctx context.Context, chatLog, systemPrompt string) (string, error) {
	logutil.Info("发送给大模型", "systemPrompt", systemPrompt, "chatLog", chatLog)
	resp, err := adapter.Chat(ctx, []Message{
		{Role: RoleSystem, Content: systemPrompt},
		{Role: RoleUser, Content: chatLog},
	}, nil)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// Chat 多轮对话；tools 非空时下发原生 function calling 工具清单
func (adapter *OpenAIAdapter) Chat(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	req := openai.ChatCompletionRequest{
		Model:       adapter.model,
		Messages:    toOpenAIMessages(messages),
		Temperature: adapter.temp,
		TopP:        adapter.topP,
	}
	if len(tools) > 0 {
		req.Tools = toOpenAITools(tools)
		logutil.Debug("下发工具清单", "数量", len(tools))
	}
	resp, err := adapter.client.CreateChatCompletion(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("大模型调用失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("大模型返回为空")
	}
	msg := resp.Choices[0].Message
	out := &ChatResponse{Content: msg.Content}
	for _, tc := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{
			ID:        tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
		})
	}
	return out, nil
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
