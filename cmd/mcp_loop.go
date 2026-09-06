package cmd

import (
	"context"
	"errors"
	"fmt"
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

	// 实际看图硬上限：真正烧视觉 token 的是「把图回传」，不是候选列表。
	// viewLimit<=0 表示不限制（此时不会有内置 view_image 工具注入）。
	viewLimit := r.appCfg.LLMConfig.ImageMax
	viewed := 0
	// 本次回复内已展示过的图片（按 data URL 去重）：图一旦以 user 消息喂回就会留在
	// 后续所有轮次的上下文里，模型再次点名同一张无需重复载入，也不占用查看上限。
	seenImages := make(map[string]bool)

	for round := 1; ; round++ {
		roundTools := tools
		if round > maxRounds {
			// 轮数用尽：不再下发工具，强制模型直接作答
			roundTools = nil
			logutil.Warn("MCP 工具调用轮数达上限，撤回工具强制作答", "工具数", len(tools), "上限", maxRounds)
		}

		resp, err := r.llmClient.MutiChatWithTool(ctx, messages, roundTools)
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
			"结束原因", resp.FinishReason,
			"是否请求工具", len(resp.ToolCalls) > 0, "请求的工具", tcNames,
			"文本字符数", len([]rune(resp.Content)))

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
			text, imgs := r.callToolOutcome(ctx, tc)

			// 已见过的图（本次回复已作为 user 消息喂回，之后所有轮次都在上下文里）
			// 不再重复下载/回传：直接提示模型参考上面的那张。
			var fresh []llm.Image
			for _, img := range imgs {
				key := llm.DataURL(img.MIMEType, img.Data)
				if seenImages[key] {
					continue
				}
				seenImages[key] = true
				fresh = append(fresh, img)
			}
			if len(imgs) > 0 && len(fresh) == 0 {
				text = "这张图片在本次对话里已经展示过，请直接参考上面那张即可，无需重复载入。"
			}

			attach := fresh
			// 超出「本次实际看图上限」：不再回传像素，明确告知模型直接作答
			if len(fresh) > 0 && viewLimit > 0 {
				if viewed >= viewLimit {
					attach = nil
					text = fmt.Sprintf("本次回复最多实际查看 %d 张图片，已达到上限。请基于已看到的内容直接作答，不要再调用查看工具。", viewLimit)
				} else if room := viewLimit - viewed; len(fresh) > room {
					attach = fresh[:room]
					text = fmt.Sprintf("已达到本次查看上限（%d 张），只展示其中部分，请据此作答。", viewLimit)
				}
				viewed += len(attach)
			}

			// 网关要求非 assistant 消息必须带 content：工具纯图片/纯数据、无文本时给个兜底说明
			if strings.TrimSpace(text) == "" {
				text = "（工具已返回内容，无文本；结果见随后的图片/数据消息）"
			}
			messages = append(messages, llm.Message{
				Role:       llm.RoleTool,
				Content:    text,
				ToolCallID: tc.ID,
				Name:       tc.Name,
			})
			// 新图片以一条 user 多模态消息喂回：放 role=tool 之后、role=user，
			// 适配 llama.cpp 这类「视觉只认 user 消息图片」的实现；一旦喂回即常驻后续轮次上下文。
			if len(attach) > 0 {
				urls := make([]string, 0, len(attach))
				for _, img := range attach {
					urls = append(urls, llm.DataURL(img.MIMEType, img.Data))
				}
				messages = append(messages, llm.Message{
					Role:          llm.RoleUser,
					Content:       "（这是你刚请求查看的图片，请据此继续回答；如需再看其它图请继续调用查看工具。）",
					ImageDataURLs: urls,
				})
			}
		}
	}
}

// imageToolResultProvider 能返回图片内容的 MCP 提供者（*mcpclient.Manager 实现）。
// 用可选接口而非改 MCPProvider 定义：单测里的假 MCP 提供者不受影响。
type imageToolResultProvider interface {
	CallToolImage(ctx context.Context, name, argsJSON string) (string, []llm.Image, error)
}

// callToolOutcome 执行一次工具调用，返回回填文本与（可能的）图片。
// 失败时返回错误描述而不是向上抛错：把失败原因交给模型看，它通常会换个参数重试或直接作答。
// 文本按 max_tool_result_rune 截断，防止某个工具一次吐几十 KB 撑爆上下文。
func (r *Router) callToolOutcome(ctx context.Context, tc llm.ToolCall) (string, []llm.Image) {
	ctx, cancel := context.WithTimeout(ctx, r.appCfg.MCPConfig.ToolTimeout)
	defer cancel()

	if p, ok := r.mcp.(imageToolResultProvider); ok {
		text, imgs, err := p.CallToolImage(ctx, tc.Name, tc.Arguments)
		text = truncateRunes(text, r.appCfg.MCPConfig.MaxToolResultRune)
		if err != nil {
			logutil.Error("MCP 工具调用失败", "tool", tc.Name, "args", tc.Arguments, "err", err)
			if strings.TrimSpace(text) == "" {
				text = "工具调用失败：" + err.Error()
			}
		}
		return text, imgs
	}

	out, err := r.mcp.CallTool(ctx, tc.Name, tc.Arguments)
	out = truncateRunes(out, r.appCfg.MCPConfig.MaxToolResultRune)
	if err != nil {
		logutil.Error("MCP 工具调用失败", "tool", tc.Name, "args", tc.Arguments, "err", err)
		if out == "" {
			return "工具调用失败：" + err.Error(), nil
		}
		// 工具自己给了错误说明（MCP isError），原样交给模型比再包一层更有信息量
		return out, nil
	}
	return out, nil
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
