package speech

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"good-review-master/config"
	"good-review-master/conversation"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/telemetry"
)

// 生成：选窗 → 拼 user 消息 → 调模型（有工具就进多轮）→ 排版。
// 生成只产出 Reply，不知道它会被发到哪；交付在 deliver.go。

// Generate 的失败原因。分得细是因为"要不要给用户一句兜底话术"要按原因区分：
// 大模型挂了该说一声，模型回了空、或窗口压根没有就别发了。
var (
	// errEmptyWindow 窗口为空。调用方都检查过窗口非空，走到这里说明上游逻辑变了，
	// 宁可这一条不发，也别发空上下文。
	errEmptyWindow = errors.New("发送窗口为空")
	// errNoHistory 缓存里一条聊天记录都没有。
	errNoHistory = errors.New("暂无群聊记录")
	// errTriggerGone 冻结快照里找不到触发消息（消息已被挤出窗口）。
	errTriggerGone = errors.New("快照里找不到触发消息")
	// errLLMFailed 大模型调用失败。
	errLLMFailed = errors.New("大模型调用失败")
)

// replyParams 一次生成的输入。窗口可以在进来之前就定好（主动链路是裁判之前定的，
// 裁判和生成看的就是同一段聊天记录）——window 非 nil 时本函数不再选窗。
type replyParams struct {
	event         onebot.Event
	groupID       string
	cfg           *config.Config // 配置快照；nil 时现取
	systemPrompt  string
	keywordPrompt string
	mention       bool
	images        []llm.Image
	// snapshot 调用方冻结的聊天记录；nil 表示现读缓存。
	snapshot []conversation.Message
	// window 已经选好的窗口；nil 表示由 Generate 自己选。
	window []conversation.Message
	// verdict 主动链路已经拿到的裁判结论；直答链路传 nil，由 Generate 自己补一次轻量判定。
	verdict *judgeVerdict
}

// Generate 生成一条回复：定窗口 → 判 long_form → 调模型 → 排版。不发送任何东西。
func (r *Router) Generate(ctx context.Context, p replyParams) (*Reply, error) {
	cfg := p.cfg
	if cfg == nil {
		cfg = r.appCfg()
	}

	window := p.window
	if window == nil {
		msgs := p.snapshot
		if msgs == nil {
			msgs = r.cacheStore().GetGroupCache(p.groupID, cfg.MaxCacheMsg).GetAll()
		}
		if p.snapshot != nil && conversation.FindMsgIndex(p.snapshot, p.event.MessageID) < 0 {
			logutil.Warn("快照里找不到触发消息，放弃本次回复", "group", p.groupID, "message", p.event.MessageID)
			return nil, errTriggerGone
		}
		if len(msgs) == 0 {
			return nil, errNoHistory
		}
		// 只处理触发消息之前的窗口：@ 触发后选窗前新到的消息不属于本次请求，
		// 否则"只回最后一条点名消息"会指到别人的新消息上。
		msgs = cutAtMessage(msgs, p.event.MessageID)
		// 先选窗再判 long_form：这轮轻量判定和正式生成看的是同一份窗口。
		// 反过来先判定再选窗，两次选窗之间缓存可能又进了新消息，两边条数就对不上了。
		window = r.selectChatWindow(msgs, p.groupID, p.systemPrompt, cfg)
	}
	if len(window) == 0 {
		logutil.Warn("发送窗口为空，放弃本次回复", "group", p.groupID)
		return nil, errEmptyWindow
	}

	var longForm bool
	if p.mention {
		// 直答链路（@/私聊）在这里补一次轻量判定，只取 long_form：
		// 知识型长篇不再强制排版；reply 结论忽略（被点名必须回）。
		longForm = r.judgeLongForm(ctx, p.event, p.groupID, cfg, window)
	} else if p.verdict != nil {
		longForm = p.verdict.LongForm
	}

	chatLog := conversation.BuildChatLog(window)
	// 只有调模型这一段吃主调用的超时；排版另算（见下），否则主调用跑得久时
	// 排版只剩一点预算，而它失败会退回原文——相当于白白少排一次版。
	genCtx, cancel := context.WithTimeout(ctx, cfg.LLMTimeout)
	defer cancel()
	userMsg := BuildUserMsg(chatLog, p.keywordPrompt)
	// agent「看图」：窗口内有图片且 view_image 工具在线时，附上候选图列表（懒加载，图不随文本常驻）
	if cfg.LLMConfig.ImageMax > 0 && r.mcp != nil && len(r.mcp.Tools()) > 0 {
		if block := r.imageCandidatesBlock(window); block != "" {
			userMsg += "\n" + block
		}
	}
	// 主动插话的候选消息图片由裁判下载好，随本次请求直接附上；
	// 必须明确告诉模型"图已经给你了"，否则它只会看到聊天记录里的"[图片]"占位。
	if len(p.images) > 0 {
		userMsg += "\n【图片】最后一条消息附带图片，已随本次请求提供，请结合图片内容回答。"
	}

	draft, err := r.runReviewLLM(genCtx, p.systemPrompt, userMsg, p.images)
	if err != nil {
		logutil.Error("大模型调用失败", "err", err, "group", p.groupID)
		return nil, fmt.Errorf("%w: %w", errLLMFailed, err)
	}
	if strings.TrimSpace(draft) == "" {
		return nil, errEmptyReply
	}
	// 最终稿再排一次版：大模型容易写成一大段，这里用 JSON schema 强制拆成最多 N 句短句。
	// 知识型长篇（longForm）跳过：那类问题是真的需要完整展开。
	// 用 ctx 而不是上面那个已带主调用超时的 genCtx，排版才有自己独立的时间预算；
	// 排版失败会退回原文，不影响这次回复。
	var sentences []string
	if longForm {
		logutil.Info("知识型长篇回复，跳过排版", "group", p.groupID)
		sentences = []string{draft}
	} else {
		sentences = r.rewriteReply(ctx, cfg, draft, chatLog, cfg.BotNickname, p.event.BotID())
	}

	return &Reply{
		Sentences: sentences,
		LongForm:  longForm,
		Event:     p.event,
		ChatID:    p.groupID,
		Mention:   p.mention,
		// 锚点记的是"这次发出去的窗口从哪条到哪条"，只有整次发送完成才推进。
		Anchor: &conversation.LLMAnchor{
			Start:    window[0].MessageID,
			LastSent: window[len(window)-1].MessageID,
			At:       time.Now(),
		},
	}, nil
}

// replyWithWindow 生成一条回复并交付到聊天渠道，返回是否发了出去。
// 生成失败时按原因决定要不要给一句兜底话术：被点名的必须有个回应，不能静默。
func (r *Router) replyWithWindow(ctx context.Context, p replyParams) bool {
	reply, err := r.Generate(ctx, p)
	if err != nil {
		if fallback := fallbackReply(p, err); fallback != nil {
			if deliverErr := r.Deliver(ctx, fallback, SinkChat); deliverErr != nil {
				logutil.Warn("兜底话术发送失败", "group", p.groupID, "err", deliverErr)
			}
		}
		return false
	}
	if err := r.Deliver(ctx, reply, SinkChat); err != nil {
		logutil.Warn("回复未完全发送，等待重试", "group", p.groupID, "err", err)
		return false
	}
	return true
}

// fallbackReply 生成失败时对被点名的那条给一句能看见的回应。
// 群里的主动接话不走这里：没被点名时失败就当没听见，比"罢工了"更合适。
// 兜底话术不带锚点——它没有对应的窗口，推进锚点会让下一轮以为那段前缀已经发过。
func fallbackReply(p replyParams, err error) *Reply {
	if !p.mention {
		return nil
	}
	text := "大师今天罢工了，稍后再试~"
	switch {
	case errors.Is(err, errNoHistory):
		text = "暂无群聊记录，无法锐评~"
	case errors.Is(err, errLLMFailed):
	default:
		// 窗口为空、快照里找不到触发消息、模型回了空：都不是"该说句话"的场合
		return nil
	}
	return &Reply{
		Sentences: []string{text},
		Event:     p.event,
		ChatID:    p.groupID,
		Mention:   true,
	}
}

// chatReview 异步锐评（通过 async 管理生命周期，自动继承 shutdown context）。
//
// parent 是分发链路的 ctx：用 GoCtx 而不是 Go，任务才能同时继承
// 调用方的 trace span 与 Group 的关闭信号（详见 async.Group.GoCtx）。
func (r *Router) chatReview(parent context.Context, event onebot.Event, groupID string, systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	logutil.InfoCtx(parent, "触发锐评", "group", groupID, "user", r.nameOf(groupID, event.UserID), "user_id", event.UserID)
	// 一次锐评是一个完整的业务动作，全程用同一份配置：
	// 缓冲上限、超时、看图开关、成本参数若来自不同次读取，算出来的窗口就没有意义了
	r.GoCtx(parent, func(ctx context.Context) error {
		r.replyWithWindow(ctx, replyParams{
			event: event, groupID: groupID,
			systemPrompt: systemPrompt, keywordPrompt: keywordPrompt, mention: true,
		})
		return nil
	})
}

// selectChatWindow 按 token 成本决定扩展还是重置，返回本次要发送的窗口消息。
// 决策逻辑在 decideChatWindow（chat_window.go，纯函数，可单测穷举）；这里只做
// 配置/锚点读取 + 日志输出。扩展条件：锚点可用、扩展成本 < 重置成本、未超上下文护栏。
func (r *Router) selectChatWindow(msgs []conversation.Message, groupID string, systemPrompt string, cfg *config.Config) []conversation.Message {
	// systemTokens = 固定前缀 P（systemPrompt + 常量前缀 + MCP 工具清单）的 token 数：扩展/重置都会发送。
	// 工具清单必须算进来：它是随请求一起下发的 tools 字段，既占上下文长度又属于缓存前缀，
	// 漏算会低估总 token，把窗口扩到撞穿 max_context_tokens 护栏。
	systemTokens := conversation.EstimateTokens(systemPrompt) + r.mcpToolsTokens()

	// 锚点过了缓存有效期就跟没有一样：服务端那段 KV 早没了，再按"命中前缀"去算
	// 扩展成本，算出来的是假账——真实情况是几万 token 全额重算，比重置还贵。
	// 宁可退回重置窗口，也不拿一个已经失效的锚点做决策。
	anchor := r.cacheStore().GetLLMAnchor(groupID)
	if anchor.Expired(cfg.CacheTTL, time.Now()) {
		logutil.Debug("锚点已过期，按无锚点处理", "group", groupID,
			"已过", time.Since(anchor.At).Round(time.Second), "有效期", cfg.CacheTTL)
		anchor = nil
	}
	decision := decideChatWindow(msgs, anchor,
		cfg.LLMConfig.CacheHitCost, cfg.LLMConfig.CacheMissCost,
		cfg.LLMConfig.MaxContextTokens, cfg.LLMSendCount, systemTokens)

	// 选窗是这套缓存成本模型里唯一的决策点，指标就埋在这里。
	// llm_cost_total 记的是**决策时的估算成本**，不是账单：真实计费口径只有服务端知道，
	// 而本项目配置里的单价本来就是相对值（见 docs/cache-cost-analysis.md）。
	// 它的用途是对比两种窗口模式的走势，不是算钱。
	telemetry.LLMWindowModeTotal.WithLabelValues(r.workspace, decision.Mode).Inc()
	if decision.Mode == "extend" {
		telemetry.LLMCacheHitTotal.WithLabelValues(r.workspace).Add(float64(decision.HitTokens))
		telemetry.LLMCostTotal.WithLabelValues(r.workspace).Add(decision.ExtendCost)
	} else {
		telemetry.LLMCostTotal.WithLabelValues(r.workspace).Add(decision.ResetCost)
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
// images 非空时以多模态消息下发（目前只有主动插话会传：裁判已下载过的最新图片）。
func (r *Router) runReviewLLM(ctx context.Context, systemPrompt, userMsg string, images []llm.Image) (string, error) {
	if r.mcp != nil && len(r.mcp.Tools()) > 0 {
		return r.runChatWithTools(ctx, systemPrompt, userMsg, images)
	}
	if r.mcp == nil {
		logutil.Info("MCP 未注入：路由器没有 MCP 提供者，走单轮调用")
	} else {
		logutil.Info("MCP 未注入：当前无在线可注入工具（服务全挂或 inject 全关），走单轮调用")
	}
	if len(images) == 0 {
		return r.llmClient.SingleChat(ctx, userMsg, systemPrompt)
	}

	dataURLs := make([]string, 0, len(images))
	for _, img := range images {
		dataURLs = append(dataURLs, llm.DataURL(img.MIMEType, img.Data))
	}
	resp, err := r.llmClient.MutiChatWithTool(ctx, []llm.Message{
		{Role: llm.RoleSystem, Content: systemPrompt},
		{Role: llm.RoleUser, Content: userMsg, ImageDataURLs: dataURLs},
	}, nil)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// mcpToolsTokens MCP 工具清单折算的 token 数（未启用时 0）
func (r *Router) mcpToolsTokens() int {
	if r.mcp == nil {
		return 0
	}
	return r.mcp.ToolsTokens()
}

// imageCandidatesBlock 组装发给模型的「本次窗口内图片候选」文本块：把发送窗口里带图消息的
// 所有图片 url 都列出来（url 只是文本，成本低），并附上全局图库里的内容/信息量描述，
// 由模型据此决定是否调 view_image 实际查看。
// url 永远取消息自己的 Images（当前聊天记录）；图库只复用描述，不存也不复用文件地址——
// 旧消息的图可能已被清理，按 message_id 加图库 id 对齐即可。
// 真正的开销控制是「实际查看上限 cfg.ImageMax」（见 runChatWithTools 的硬限）。无候选返回空串。
func (r *Router) imageCandidatesBlock(msgs []conversation.Message) string {
	type row struct {
		nick    string
		msgID   string
		url     string
		note    conversation.ImageNote
		hasNote bool
	}
	rows := make([]row, 0)
	for _, m := range msgs {
		for i, u := range m.Images {
			if u == "" {
				continue
			}
			// 这里标的是聊天记录里的 message_id（即库里的行号），模型才能把图和那一行对上。
			msgID := m.MessageID
			if m.RowID > 0 {
				msgID = strconv.FormatInt(m.RowID, 10)
			}
			item := row{nick: m.Nickname, msgID: msgID, url: u}
			if i < len(m.ImageNotes) {
				note := m.ImageNotes[i]
				if note.Status == "ok" && note.Note != "" {
					item.note, item.hasNote = note, true
				}
			}
			rows = append(rows, item)
		}
	}
	if len(rows) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n【群内图片候选（共 ")
	b.WriteString(fmt.Sprintf("%d", len(rows)))
	b.WriteString(" 张）】候选只是文字，查看某张图才会真正载入像素并占用上下文。先看每行末尾的内容/信息量：")
	b.WriteString("信息量 low/none、或类型是表情包/梗图的，不要看；信息量 high/medium、或消息文字在问图里的东西，且回复需要其中信息时，才调用 view_image。")
	b.WriteString("url 必须从下面逐字复制（文件名里的时间戳不要改写、不要猜测），改写会导致文件不存在；")
	if viewMax := r.appCfg().LLMConfig.ImageMax; viewMax > 0 {
		b.WriteString(fmt.Sprintf("本次最多实际查看 %d 张，达到上限后请直接基于已看到的内容作答；", viewMax))
	}
	b.WriteString("没有描述的图按聊天文字判断，拿不准但需要回应时最多看 1 张；不要编造没实际看过的图片内容。\n")
	for i, row := range rows {
		line := fmt.Sprintf("图%d 发送者:%s (消息%s) url:%s", i+1, row.nick, row.msgID, row.url)
		if row.hasNote {
			line += fmt.Sprintf(" 内容:%s 信息量:%s", row.note.Note, row.note.InfoLevel)
			if row.note.HasText {
				line += " 文字:有"
			}
		}
		b.WriteString(line + "\n")
	}
	return strings.TrimSpace(b.String())
}

// BuildUserMsg 组装发给大模型的 user message：聊天记录 + 该指令的提示词。
// 「按最后一条消息回复/执行」与工具使用的固定要求只放在 system prompt（见 replyRequirement
// 与 composeSystemPrompt），这里不再重复，避免同一条指令发两遍。
// 人格块不在 user 消息里：由调用方渲染后交给 ComposeSystemPrompt 拼在最前（关键字路由传 route.Persona，直答与主动接话传 groupPersonaBlock）。
//
// 导出是给 tests/eval 用的（与 ComposeSystemPrompt 同理）：离线评测必须看到与线上逐字相同的输入。
//
// **注意一处与文档不符的现状**：本函数原先还接收 `mentionerNick` 与 `extra` 两个参数，
// 但函数体从未用过它们——也就是说"关键字后面的补充文本"目前**没有**拼进提示词里，
// 这次只是把两个死参数删掉（纯机械清理，行为零变化），没有顺手把它加回去：
// 那会改变发给大模型的内容，属于产品决定，该由人来拍板。
func BuildUserMsg(chatLog string, keywordPrompt string) string {
	return chatLog + "\n" + keywordPrompt + "\n"
}
