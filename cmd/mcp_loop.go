package cmd

import (
	"context"
	"errors"
	"strings"

	"good-review-master/llm"
	"good-review-master/logutil"
)

// errEmptyReply 模型既没给文本也没给工具调用（或强制作答后仍为空）
var errEmptyReply = errors.New("大模型返回空内容")

// MCPProvider 对话期需要的 MCP 能力（由 *mcpclient.Manager 实现）。
// 用接口而非具体类型：MCP 未启用时可以直接传 nil，单测也能塞假实现穷举循环分支。
type MCPProvider interface {
	// Tools 当前可注入的工具清单快照（按名排序，发布后不变）
	Tools() []llm.Tool
	// ToolsTokens 清单折算的 token 数，需计入缓存成本模型的固定前缀
	ToolsTokens() int
	// CallTool 按暴露名调用工具，返回拍平后的结果文本
	CallTool(ctx context.Context, name, argsJSON string) (string, error)
}

// runChatWithTools 带 MCP 工具的对话循环（原生 function calling）：
// 下发工具清单 → 模型要么直接给文本答案（结束），要么要求调工具 →
// 后端执行 tools/call，把结果作为 role=tool 消息回填 → 再问一轮，
// 直到拿到文本答案或用完 max_tool_rounds。
//
// 成本说明：每轮都会重发完整上下文（system + 聊天记录 + 历轮工具结果），
// 但第 2 轮起前缀与上一轮完全一致，走大模型的 prefix cache 命中价，边际成本很低；
// 真正的新增开销只有 assistant 的 tool_calls 和 tool 结果那几条消息。
// 轮数用尽后撤回 tools 字段，模型只能基于已拿到的信息直接作答，杜绝无限调用。
func (r *Router) runChatWithTools(ctx context.Context, systemPrompt, userMsg string) (string, error) {
	tools := r.mcp.Tools()
	maxRounds := r.appCfg.MCPConfig.MaxToolRounds
	logutil.Info("对话走 MCP 工具循环", "注入工具数", len(tools), "最大轮数", maxRounds, "工具token", r.mcp.ToolsTokens())
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt},
		{Role: llm.RoleUser, Content: userMsg},
	}

	for round := 1; ; round++ {
		roundTools := tools
		if round > maxRounds {
			// 轮数用尽：不再下发工具，强制模型直接作答
			roundTools = nil
			logutil.Warn("MCP 工具调用轮数达上限，撤回工具强制作答", "工具数", len(tools), "上限", maxRounds)
		}

		resp, err := r.llmClient.Chat(ctx, messages, roundTools)
		if err != nil {
			return "", err
		}

		// 模型原始返回全量打出来（内容截断到 500 字，工具调用参数原样）：
		// 靠它确认模型到底是「没收到工具」还是「收到了却不调」
		tcNames := make([]string, 0, len(resp.ToolCalls))
		for _, tc := range resp.ToolCalls {
			tcNames = append(tcNames, tc.Name)
		}
		logutil.Info("模型本轮原始返回", "轮次", round, "下发工具数", len(roundTools),
			"是否请求工具", len(resp.ToolCalls) > 0, "请求的工具", tcNames,
			"文本字符数", len([]rune(resp.Content)), "文本摘录", truncateRunes(strings.TrimSpace(resp.Content), 500))

		if len(resp.ToolCalls) == 0 || roundTools == nil {
			// 没要求调工具 = 最终答案；已撤回工具却仍返回 tool_calls 的模型也在这里收口
			if strings.TrimSpace(resp.Content) == "" {
				return "", errEmptyReply
			}
			return resp.Content, nil
		}

		logutil.Info("模型请求调用 MCP 工具", "轮次", round, "调用数", len(resp.ToolCalls))
		messages = append(messages, llm.Message{
			Role:      llm.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})
		// 同一轮的多个调用串行执行：绝大多数场景一轮只有一个调用，
		// 串行能让回填顺序与 tool_calls 顺序严格一致，也避免并发打爆同一个 MCP 服务
		for _, tc := range resp.ToolCalls {
			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				Content:    r.callMCPTool(ctx, tc),
				ToolCallID: tc.ID,
				Name:       tc.Name,
			})
		}
	}
}

// callMCPTool 执行一次工具调用，返回要回填给模型的文本。
// 失败时返回错误描述而不是向上抛错：把失败原因交给模型看，它通常会换个参数重试或直接作答。
// 结果按 max_tool_result_rune 截断，防止某个工具一次吐几十 KB 撑爆上下文。
func (r *Router) callMCPTool(ctx context.Context, tc llm.ToolCall) string {
	ctx, cancel := context.WithTimeout(ctx, r.appCfg.MCPConfig.ToolTimeout)
	defer cancel()

	out, err := r.mcp.CallTool(ctx, tc.Name, tc.Arguments)
	out = truncateRunes(out, r.appCfg.MCPConfig.MaxToolResultRune)
	if err != nil {
		logutil.Error("MCP 工具调用失败", "tool", tc.Name, "args", tc.Arguments, "err", err)
		if out == "" {
			return "工具调用失败：" + err.Error()
		}
		// 工具自己给了错误说明（MCP isError），原样交给模型比再包一层更有信息量
		return out
	}
	return out
}

// truncateRunes 按字符数截断，超长时补省略号（与 bot 层截断群消息的写法一致）
func truncateRunes(s string, max int) string {
	if max <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + "..."
}
