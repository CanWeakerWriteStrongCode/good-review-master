package router

import (
	"context"
	"fmt"
	"strings"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/telemetry"
)

// chatReview 异步锐评（通过 async 管理生命周期，自动继承 shutdown context）。
//
// parent 是分发链路的 ctx：用 GoCtx 而不是 Go，任务才能同时继承
// 调用方的 trace span 与 Group 的关闭信号（详见 async.Group.GoCtx）。
func (r *Router) chatReview(parent context.Context, event onebot.Event, groupID string, systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	logutil.InfoCtx(parent, "触发锐评", "group", groupID, "user", event.Nickname)
	// 一次锐评是一个完整的业务动作，全程用同一份配置：
	// 缓冲上限、超时、看图开关、成本参数若来自不同次读取，算出来的窗口就没有意义了
	cfg := r.appCfg()
	r.GoCtx(parent, func(ctx context.Context) error {
		msgs := cache.GetGroupCache(groupID, cfg.MaxCacheMsg).GetAll()
		if len(msgs) == 0 {
			r.obClient.SendGroupMessage(groupID, "暂无群聊记录，无法锐评~")
			return nil
		}

		// 按 token 成本决定扩展/重置，得到本次要发送的窗口
		chatLogMsgs := r.selectChatWindow(msgs, groupID, systemPrompt, cfg)
		chatLog := cache.BuildChatLog(chatLogMsgs)

		ctx, cancel := context.WithTimeout(ctx, cfg.LLMTimeout)
		defer cancel()
		userMsg := BuildUserMsg(chatLog, keywordPrompt)
		// agent「看图」：窗口内有图片且 view_image 工具在线时，附上候选图列表（懒加载，图不随文本常驻）
		if cfg.LLMConfig.ImageMax > 0 && r.mcp != nil && len(r.mcp.Tools()) > 0 {
			if block := r.imageCandidatesBlock(chatLogMsgs); block != "" {
				userMsg += "\n" + block
			}
		}

		reply, err := r.runReviewLLM(ctx, systemPrompt, userMsg)
		if err != nil {
			logutil.Error("大模型调用失败", "err", err)
			r.obClient.SendGroupMessage(groupID, "大师今天罢工了，稍后再试~")
			return nil
		}
		r.obClient.SendGroupMessage(groupID, reply)

		// 保存锚点（窗口首条 + 末条 MsgID）
		cache.SetLLMAnchor(groupID, cache.LLMAnchor{
			Start:    chatLogMsgs[0].MsgID,
			LastSent: chatLogMsgs[len(chatLogMsgs)-1].MsgID,
		})
		return nil
	})
}

// selectChatWindow 按 token 成本决定扩展还是重置，返回本次要发送的窗口消息。
// 决策逻辑在 decideChatWindow（router/chat_window.go，纯函数，可单测穷举）；这里只做
// 配置/锚点读取 + 日志输出。扩展条件：锚点可用、扩展成本 < 重置成本、未超上下文护栏。
func (r *Router) selectChatWindow(msgs []cache.Message, groupID string, systemPrompt string, cfg *config.Config) []cache.Message {
	// systemTokens = 固定前缀 P（systemPrompt + 常量前缀 + MCP 工具清单）的 token 数：扩展/重置都会发送。
	// 工具清单必须算进来：它是随请求一起下发的 tools 字段，既占上下文长度又属于缓存前缀，
	// 漏算会低估总 token，把窗口扩到撞穿 max_context_tokens 护栏。
	systemTokens := cache.EstimateTokens(systemPrompt) + r.mcpToolsTokens()
	decision := decideChatWindow(msgs, cache.GetLLMAnchor(groupID),
		cfg.LLMConfig.CacheHitCost, cfg.LLMConfig.CacheMissCost,
		cfg.LLMConfig.MaxContextTokens, cfg.LLMSendCount, systemTokens)

	// 选窗是这套缓存成本模型里唯一的决策点，指标就埋在这里。
	// llm_cost_total 记的是**决策时的估算成本**，不是账单：真实计费口径只有服务端知道，
	// 而本项目配置里的单价本来就是相对值（见 docs/cache-cost-analysis.md）。
	// 它的用途是对比两种窗口模式的走势，不是算钱。
	telemetry.LLMWindowModeTotal.WithLabelValues(decision.Mode).Inc()
	if decision.Mode == "extend" {
		telemetry.LLMCacheHitTotal.Add(float64(decision.HitTokens))
		telemetry.LLMCostTotal.Add(decision.ExtendCost)
	} else {
		telemetry.LLMCostTotal.Add(decision.ResetCost)
	}

	if decision.Mode == "extend" {
		logutil.Debug("缓存扩展", "group", groupID, "窗口", len(decision.Window),
			"命中token", decision.HitTokens, "新增token", decision.NewTokens, "重置token", decision.ResetTokens,
			"扩展成本", decision.ExtendCost, "重置成本", decision.ResetCost)
	} else {
		logutil.Debug("缓存重置("+decision.ResetReason+")", "group", groupID, "窗口", len(decision.Window),
			"重置token", decision.ResetTokens)
	}
	return decision.Window
}

// runReviewLLM 按当前是否有可用 MCP 工具决定走工具循环还是单轮调用。
// 工具清单为空（MCP 未启用 / 服务全挂 / inject 全关）时走原来的单轮路径，
// 请求体里不带 tools 字段，与改造前的字节完全一致，不会无故击穿缓存。
func (r *Router) runReviewLLM(ctx context.Context, systemPrompt, userMsg string) (string, error) {
	if r.mcp != nil && len(r.mcp.Tools()) > 0 {
		return r.runChatWithTools(ctx, systemPrompt, userMsg)
	}
	if r.mcp == nil {
		logutil.Info("MCP 未注入：路由器没有 MCP 提供者，走单轮调用")
	} else {
		logutil.Info("MCP 未注入：当前无在线可注入工具（服务全挂或 inject 全关），走单轮调用")
	}
	return r.llmClient.SingleChat(ctx, userMsg, systemPrompt)
}

// mcpToolsTokens MCP 工具清单折算的 token 数（未启用时 0）
func (r *Router) mcpToolsTokens() int {
	if r.mcp == nil {
		return 0
	}
	return r.mcp.ToolsTokens()
}

// imageCandidatesBlock 组装发给模型的「本次窗口内图片候选」文本块：把发送窗口里带图消息的
// 所有图片 url 都列出来（url 只是文本，成本低），由模型自行决定调 view_image 实际查看哪张。
// 真正的开销控制是「实际查看上限 cfg.ImageMax」（见 runChatWithTools 的硬限）。无候选返回空串。
func (r *Router) imageCandidatesBlock(msgs []cache.Message) string {
	if len(msgs) == 0 {
		return ""
	}
	type row struct {
		nick  string
		msgID int64
		url   string
	}
	var rows []row
	for _, m := range msgs {
		for _, u := range m.Images {
			if u == "" {
				continue
			}
			rows = append(rows, row{nick: m.Nick, msgID: m.MsgID, url: u})
		}
	}
	if len(rows) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n【群内图片候选（共 ")
	b.WriteString(fmt.Sprintf("%d", len(rows)))
	b.WriteString(" 张）】候选只是文字，查看某张图才会真正载入像素。需要看图来回答（图片/表情/截图/图内文字）时，调用 view_image 工具并传对应 url；")
	if viewMax := r.appCfg().LLMConfig.ImageMax; viewMax > 0 {
		b.WriteString(fmt.Sprintf("本次最多实际查看 %d 张，达到上限后请直接基于已看到的内容作答；", viewMax))
	}
	b.WriteString("不要编造没实际看过的图片内容。\n")
	for i, row := range rows {
		b.WriteString(fmt.Sprintf("图%d 发送者:%s (消息%d) url:%s\n", i+1, row.nick, row.msgID, row.url))
	}
	return strings.TrimSpace(b.String())
}

// BuildUserMsg 组装发给大模型的 user message：聊天记录 + 固定要求 + 该指令的提示词。
// 人格块不在 user 消息里：由调用方渲染后拼进 systemPrompt（RouteMessage 关键字路由 / replyDefault 群人格）。
//
// 导出是给 tests/eval 用的（与 ComposeSystemPrompt 同理）：离线评测必须看到与线上逐字相同的输入。
//
// **注意一处与文档不符的现状**：本函数原先还接收 `mentionerNick` 与 `extra` 两个参数，
// 但函数体从未用过它们——也就是说"关键字后面的补充文本"目前**没有**拼进提示词里，
// 而 CLAUDE.md 把"extra 会作为「用户补充,优先级很高」追加"写成了既有行为。
// 这次只是把两个死参数删掉（纯机械清理，行为零变化），没有顺手把它加回去：
// 那会改变发给大模型的内容，属于产品决定，该由人来拍板。
func BuildUserMsg(chatLog string, keywordPrompt string) string {
	userMsg := chatLog + "\n"
	userMsg += "【重点】优先回复或者执行最后一条信息【工具使用】当用户询问真实信息时，应调用对应MCP工具，禁止自行编造答案或推脱"
	userMsg += keywordPrompt + "\n"
	return userMsg
}
