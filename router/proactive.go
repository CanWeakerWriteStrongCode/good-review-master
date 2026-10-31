package router

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/mcpserver"
	"good-review-master/onebot"
	"good-review-master/telemetry"
)

// 主动插话：没有被 @ 时，先让一个轻量"裁判"调用判断要不要接话，
// 判定为要才走与普通 @ 相同的生成链路。设计上的四个关键点：
//
//  1. 两阶段：裁判只输出 {reply, reason}，不带人格、不带 MCP 工具；
//     绝大多数消息止步于此，成本远低于每次都跑完整锐评。
//  2. 默认沉默：解析失败、超时、报错一律按"不回"处理（fail closed）。
//  3. 每 (工作空间, 群) 串行：Router 本来就是每工作空间一个，所以这里的
//     状态表按 groupID 建键即可。运行期间新消息只覆盖单槽 pending（latest-wins），
//     不排队、不并发——群里刷屏时旧消息不会攒成一串过期判定。
//  4. 三重限流：发言后冷却、判定最小间隔、同一话题连续发言上限。
//
// 显式 @ 的回复不经过这里：handler 会直接走 RouteMessage → chatReview；
// TryProactive 自己也会再检查一次 mention 并转给 RouteMessage，点名绝不进裁判。

// proactiveSilenceReset 群静默超过这个时长视为新话题，连续发言计数清零。
const proactiveSilenceReset = 5 * time.Minute

// 裁判 focus 字段的两种取值（与 schema 的 enum、裁判提示词里的说明保持一致）。
const (
	proactiveFocusContext = "整体上下文"
	proactiveFocusLast    = "最后一条消息"
)

// proactiveVerdictSchema 裁判输出的 JSON Schema（strict 模式的硬要求：
// 所有属性都进 required、additionalProperties 必须是 false）。
var proactiveVerdictSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "reply":  {"type": "boolean", "description": "机器人是否应该主动回复最后一条消息"},
    "long_form": {"type": "boolean", "description": "是否需要查资料或正式知识型长篇解答（允许超过排版句数上限）"},
    "focus":  {"type": "string", "enum": ["整体上下文", "最后一条消息"], "description": "回复焦点：看整体上下文，还是只聚焦最后一条消息"},
    "reason": {"type": "string",  "description": "不超过 20 字的理由"}
  },
  "required": ["reply", "long_form", "focus", "reason"],
  "additionalProperties": false
}`)

// judgeVerdict 裁判结论。Reply 用指针：区分"模型明确说 false"和"字段缺失"。
// Focus 决定生成阶段按哪种口径组织回复；缺失或无法识别时按整体上下文处理。
// LongForm 表示这条需要查资料/正式知识型长篇解答，生成后跳过排版（不强制 ≤N 句）。
type judgeVerdict struct {
	Reply    *bool  `json:"reply"`
	LongForm bool   `json:"long_form"`
	Focus    string `json:"focus"`
	Reason   string `json:"reason"`
}

func (v judgeVerdict) shouldReply() bool { return v.Reply != nil && *v.Reply }

// focusLast 裁判是否要求生成时聚焦最后一条消息。
func (v judgeVerdict) focusLast() bool { return v.Focus == proactiveFocusLast }

// judgeNo 构造一个明确的"不回"结论（本地兜底时用，理由进日志）。
func judgeNo(reason string) judgeVerdict {
	no := false
	return judgeVerdict{Reply: &no, Reason: reason}
}

// proactiveCandidate 一个等待判定的消息（可能是运行期间被覆盖后剩下的最新一条）。
type proactiveCandidate struct {
	event    onebot.Event
	content  string
	snapshot []cache.Message // 提交时的冻结窗口；nil = 判定时现读缓存
}

// proactiveGroup 一个机器人在一个群里的主动插话状态。
type proactiveGroup struct {
	running     bool                // 有 worker 在跑（覆盖"判定 + 生成"两段）
	pending     *proactiveCandidate // 单槽：运行期间新消息只覆盖，不排队
	lastJudgeAt time.Time           // 上次判定**开始**时刻；判定间隔从这一刻算，含本轮大模型耗时
	lastReplyAt time.Time           // 上次发言时刻（冷却）
	lastReply   string              // 上次发言摘要（裁判输入）
	streak      int                 // 当前话题里连续主动发言次数
	lastActive  time.Time           // 群里最后一次活动（静默重置 streak）
}

// proactiveRegistry 主动插话状态表。Router 每个工作空间一份，
// 所以键只有 groupID 时语义就是 (workspace, group) 联合。
type proactiveRegistry struct {
	mu     sync.Mutex
	groups map[string]*proactiveGroup
}

// group 取（必要时创建）某个群的状态；调用方必须持锁。
func (p *proactiveRegistry) group(groupID string) *proactiveGroup {
	if p.groups == nil {
		p.groups = make(map[string]*proactiveGroup)
	}
	g := p.groups[groupID]
	if g == nil {
		g = &proactiveGroup{}
		p.groups[groupID] = g
	}
	return g
}

// enqueue 把一条消息交给主动链路。同步、零大模型调用。
//
// 返回 true 表示当前没有 worker，调用方负责起 goroutine；
// 返回 false 表示已被拒（限流/上限）或已合并进运行中的 worker。
func (p *proactiveRegistry) enqueue(groupID string, cand *proactiveCandidate, cfg *config.Config, now time.Time) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	g := p.group(groupID)
	// 静默太久视为新话题：连续发言计数清零
	if !g.lastActive.IsZero() && now.Sub(g.lastActive) > proactiveSilenceReset {
		g.streak = 0
	}
	g.lastActive = now

	// 已经有 worker 在跑：只更新单槽，冷却/上限交给 worker 出队时复查。
	// 这里不能做间隔检查——lastJudgeAt 刚被前一次判定写过，会把运行期间的消息全丢掉。
	if g.running {
		g.pending = cand
		return false
	}

	if g.streak >= cfg.ProactiveMaxStreak {
		return false
	}
	if coolingDown(g, cfg, now) {
		return false
	}
	if g.lastJudgeAt.Add(cfg.ProactiveJudgeEvery).After(now) {
		return false
	}

	// 判定间隔从"上次判定开始"算：本轮裁判+生成的耗时已经包含在间隔里。
	// 因此单轮耗时 ≥ 间隔时，下一条会立刻接上；间隔内的新消息直接丢弃，不排队延后。
	g.running = true
	g.pending = cand
	return true
}

// next 供 worker 出队：取走 pending 并复查限流。
// done=true 表示没有待处理消息、worker 应退出（running 已复位）；
// cand=nil 且 done=false 表示这条被丢弃，worker 继续看下一条。
func (p *proactiveRegistry) next(groupID string, cfg *config.Config, now time.Time) (*proactiveCandidate, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	g := p.group(groupID)
	if !g.lastActive.IsZero() && now.Sub(g.lastActive) > proactiveSilenceReset {
		g.streak = 0
	}
	g.lastActive = now

	cand := g.pending
	g.pending = nil
	if cand == nil {
		g.running = false
		return nil, true
	}
	if g.streak >= cfg.ProactiveMaxStreak || coolingDown(g, cfg, now) ||
		g.lastJudgeAt.Add(cfg.ProactiveJudgeEvery).After(now) {
		// 间隔没过：这条待判消息直接丢弃（不是留到间隔结束后补判）。
		// lastJudgeAt 是上一轮的开始时刻，所以上一轮跑得越久，这里越容易直接放行。
		return nil, false
	}
	g.lastJudgeAt = now
	return cand, false
}

// recordReply 记录一次主动发言：冷却起点、连续计数、裁判可看到的上一句。
func (p *proactiveRegistry) recordReply(groupID, text string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	g := p.group(groupID)
	g.lastReplyAt = now
	g.streak++
	g.lastReply = summarize(text)
}

// noteDirect 被点名/引用后的回复：对话对象切回了机器人，
// 连续主动发言计数清零；同时机器人刚说过话，主动链路也要等冷却。
func (p *proactiveRegistry) noteDirect(groupID, text string, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	g := p.group(groupID)
	g.streak = 0
	g.lastReplyAt = now
	g.lastReply = summarize(text)
}

// lastTurn 返回"机器人上次发言"的摘要，供裁判判断话题是否在延续。
func (p *proactiveRegistry) lastTurn(groupID string, now time.Time) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	g := p.group(groupID)
	if g.lastReply == "" || g.lastReplyAt.IsZero() {
		return ""
	}
	return humanAgo(now.Sub(g.lastReplyAt)) + "：" + g.lastReply
}

// abandon worker 退出/panic 时兜底复位，避免一个群被永久锁死。
func (p *proactiveRegistry) abandon(groupID string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	g := p.group(groupID)
	g.running = false
	g.pending = nil
}

func coolingDown(g *proactiveGroup, cfg *config.Config, now time.Time) bool {
	return !g.lastReplyAt.IsZero() && g.lastReplyAt.Add(cfg.ProactiveCooldown).After(now)
}

// TryProactive 是非 @ 消息的入口：先做零成本的代码门控，再异步判定+回复。
//
// 必须在消息已写入缓存之后调用（缓存里才有完整窗口）。
func (r *Router) TryProactive(ctx context.Context, event onebot.Event, groupID, content string) {
	r.tryProactive(ctx, event, groupID, content, nil, false)
}

// TryProactiveNow 是 Stage 3 的同步入口：提交后在当前 goroutine 里处理完 latest-wins
// 队列再返回，保证同一会话的 Agent 任务严格串行（全局并发由阶段 worker 控制）。
func (r *Router) TryProactiveNow(ctx context.Context, event onebot.Event, groupID, content string, snapshot []cache.Message) {
	r.tryProactive(ctx, event, groupID, content, snapshot, true)
}

func (r *Router) tryProactive(ctx context.Context, event onebot.Event, groupID, content string, snapshot []cache.Message, sync bool) {
	// 点名（@ 本账号）必须回答，绝不经过裁判：裁判默认沉默，问它等于可能不回。
	// handler 已在入口把 @ 分给 RouteMessage，这里再兜一层，任何调用方都不能
	// 把点名消息送进"要不要回"的判定。
	if event.MentionsBot() || event.AdapterAtMe() {
		if sync {
			r.RouteMessageNow(ctx, content, event, groupID, snapshot)
		} else {
			r.RouteMessage(ctx, content, event, groupID)
		}
		return
	}
	cfg := r.appCfg()
	if !cfg.ProactiveEnabled || groupID == "" {
		return
	}
	// 定向消息：@全体、引用某人、@ 了别的群友——都属于有明确对象的对话，
	// 主动插话不介入。回复机器人消息在 v1 里也归到"引用"一并跳过。
	if event.MentionsAll() || event.QuotedMessageID() != "" || event.QuotedRefIdx() != "" {
		return
	}
	if mentionsOther(event) {
		return
	}
	if botID := event.BotID(); botID != "" && event.UserID == botID {
		return
	}
	// 纯图片/表情消息：没有看图能力就直接放弃，省掉一次注定无意义的裁判
	if isPlaceholderOnly(content) && (cfg.ProactiveJudgeImages <= 0 || cfg.LLMConfig.ImageMax <= 0) {
		return
	}

	cand := &proactiveCandidate{event: event, content: content, snapshot: snapshot}
	if !r.proactive.enqueue(groupID, cand, cfg, time.Now()) {
		return
	}
	if sync {
		r.runProactive(ctx, groupID)
		return
	}
	r.GoCtx(ctx, func(workerCtx context.Context) error {
		// 只有异常退出（panic）才需要兜底复位：正常路径由 next() 在发现空槽时
		// 复位 running。用 done 标记避免误清掉"本 worker 结束后新启动的 worker"的状态。
		finished := false
		defer func() {
			if !finished {
				r.proactive.abandon(groupID)
			}
		}()
		r.runProactive(workerCtx, groupID)
		finished = true
		return nil
	})
}

// runProactive 每 (工作空间, 群) 的唯一 worker：串行处理 pending，直到队列空。
func (r *Router) runProactive(ctx context.Context, groupID string) {
	for {
		cfg := r.appCfg()
		cand, done := r.proactive.next(groupID, cfg, time.Now())
		if done {
			return
		}
		if ctx.Err() != nil {
			r.proactive.abandon(groupID)
			return
		}
		if cand == nil {
			continue
		}
		r.handleProactive(ctx, cand, groupID, cfg)
	}
}

// handleProactive 对一个候选消息执行"裁判 → 必要时生成并发送"。
func (r *Router) handleProactive(ctx context.Context, cand *proactiveCandidate, groupID string, cfg *config.Config) {
	// 图片只下载一次：裁判看它决定要不要回，生成阶段也直接复用这份像素，
	// 否则生成只能看到聊天记录里的“[图片]”，会出现“裁判看懂了新闻、回复却在聊构图”的割裂。
	images := r.judgeImages(ctx, cand.event, cfg)

	verdict, err := r.judgeProactive(ctx, groupID, cand, cfg, images, false)
	if err != nil {
		telemetry.ProactiveJudgeTotal.WithLabelValues(r.workspace, telemetry.ProactiveJudgeError).Inc()
		logutil.Warn("主动判定失败，本条不回复", "group", groupID, "err", err)
		return
	}
	if !verdict.shouldReply() {
		telemetry.ProactiveJudgeTotal.WithLabelValues(r.workspace, telemetry.ProactiveJudgeNo).Inc()
		logutil.Info("主动判定：不回复", "group", groupID, "reason", verdict.Reason)
		return
	}
	telemetry.ProactiveJudgeTotal.WithLabelValues(r.workspace, telemetry.ProactiveJudgeYes).Inc()
	logutil.Info("主动判定：回复", "group", groupID, "focus", verdict.Focus, "long_form", verdict.LongForm,
		"reason", verdict.Reason)

	systemPrompt := r.proactiveSystemPrompt(cfg, cand.event, groupID, verdict.focusLast())
	reply, sent := r.chatReplyWithSnapshot(ctx, cand.event, groupID, systemPrompt, "", false, images, cand.snapshot, &verdict)
	if sent {
		r.proactive.recordReply(groupID, reply, time.Now())
	}
}

// proactiveSystemPrompt 主动插话的 system prompt：在普通问答基础上换成
// "主动参与"的要求（原始提示词里那句"根据最后@你的群友"在这里不成立）。
func (r *Router) proactiveSystemPrompt(cfg *config.Config, event onebot.Event, groupID string, focusLast bool) string {
	systemPrompt := ComposeProactiveSystemPrompt(cfg, event.BotID(), "", r.promptCfg.ProactiveRequirement())
	if b, ok := r.getGroupPersona(groupID); ok {
		systemPrompt += "\n" + RenderPersona(b.persona, b.sharedRules)
	} else {
		systemPrompt += "\n当前无指定人格，现在是普通大模型问答模式：请如实正常回答，不要代入任何虚构人格或角色。"
	}
	// 裁判判了回复焦点：把它作为最后一条关键提示词，决定按整段语境还是只盯最后一句组织回答。
	systemPrompt += "\n" + r.promptCfg.FocusPrompt(focusLast)
	return systemPrompt
}

// judgeProactive 问裁判"这条要不要回"。任何失败都由调用方按"不回"处理。
// images 是已经下载好的候选图片（handleProactive 只下载一次，生成阶段复用）。
// mention=true 表示这条本身 @ 了机器人（或私聊）：reply 结论一律忽略，只取 long_form/focus。
func (r *Router) judgeProactive(ctx context.Context, groupID string, cand *proactiveCandidate, cfg *config.Config, images []llm.Image, mention bool) (judgeVerdict, error) {
	msgs := cand.snapshot
	if msgs == nil {
		msgs = r.cacheStore().GetGroupCache(groupID, cfg.MaxCacheMsg).GetAll()
	}
	// 裁判与生成看同一份窗口来源：都用 llm_send_count 条。生成阶段会按缓存命中
	// 再实时扩展/重置，这里不再单独给裁判准备一份聊天记录。
	window := judgeWindow(msgs, cand.event.MessageID, cfg.LLMSendCount)
	if len(window) == 0 {
		return judgeVerdict{}, fmt.Errorf("没有可判定的聊天记录")
	}

	// 纯图片消息但一张都没读到：裁判看不到内容，只能按"不回"处理（直答链路不受此限）
	if !mention && isPlaceholderOnly(cand.content) && len(images) == 0 {
		return judgeNo("纯图片消息未能读取到图片"), nil
	}

	dataURLs := make([]string, 0, len(images))
	for _, img := range images {
		dataURLs = append(dataURLs, llm.DataURL(img.MIMEType, img.Data))
	}
	userText := buildJudgeText(window, cfg.BotNickname, cand.event.BotID(), r.proactive.lastTurn(groupID, time.Now()), len(dataURLs) > 0, mention)
	// 裁判提示词来自 prompt_system.yaml 的 internal.proactive_judge（缺失时用内置默认值）。
	// 内容必须逐字节稳定才能持续命中 prompt 缓存；动态内容都在 user 消息里。
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: r.promptCfg.ProactiveJudgePrompt()},
		{Role: llm.RoleUser, Content: userText, ImageDataURLs: dataURLs},
	}

	timeout := cfg.ProactiveJudgeTimeout
	if timeout <= 0 {
		timeout = cfg.LLMTimeout
	}
	judgeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := r.llmClient.ChatJSON(judgeCtx, messages, proactiveVerdictSchema)
	if err != nil {
		return judgeVerdict{}, err
	}
	return parseJudgeVerdict(resp.Content)
}

// judgeLongForm 直答（@/私聊）链路的轻量判定：只取 long_form，
// reply 结论一律忽略——被点名必须回，裁判说"不回"也不作数。
//
// 返回 longForm=true 表示这条是查资料/知识型长篇解答，生成后跳过排版（不强制 ≤N 句）。
// 任何失败都返回 false（照常排版）：排版只是格式，不该因为它挡住回复。
func (r *Router) judgeLongForm(ctx context.Context, event onebot.Event, groupID string, cfg *config.Config) bool {
	// 排版已关闭时 long_form 没有消费方，省掉这次调用。
	if cfg.LLMConfig.ReplyMaxSentences <= 0 {
		return false
	}
	cand := &proactiveCandidate{event: event}
	verdict, err := r.judgeProactive(ctx, groupID, cand, cfg, nil, true)
	if err != nil {
		logutil.Warn("直答 long_form 判定失败，按普通回复排版", "group", groupID, "err", err)
		return false
	}
	if verdict.LongForm {
		logutil.Info("直答判定：知识型长篇，跳过排版", "group", groupID, "reason", verdict.Reason)
	}
	return verdict.LongForm
}

// judgeImages 读取候选消息的图（最多 cfg.ProactiveJudgeImages 张）。
// 用的是与 view_image 相同的下载器和本地目录白名单：QQ 官方图片是 AstrBot 的临时文件。
func (r *Router) judgeImages(ctx context.Context, event onebot.Event, cfg *config.Config) []llm.Image {
	urls := event.Images()
	if len(urls) == 0 || cfg.ProactiveJudgeImages <= 0 || cfg.LLMConfig.ImageMax <= 0 {
		return nil
	}
	limit := min(cfg.ProactiveJudgeImages, len(urls))
	fetcher := mcpserver.DefaultFetcher(cfg.LLMConfig.ImageLocalDirs...)

	out := make([]llm.Image, 0, limit)
	for _, url := range urls[:limit] {
		data, mime, err := fetcher.FetchImage(ctx, url)
		if err != nil {
			logutil.Warn("读取判定图片失败", "group", event.GroupID, "err", err)
			continue
		}
		out = append(out, llm.Image{Data: data, MIMEType: mime})
	}
	return out
}

// judgeWindow 取"截止到候选消息、最多 limit 条"的窗口。
// limit 用 runtime.llm_send_count，与生成阶段的重置窗口一致。
// 以候选消息为右边界：worker 判定时缓存里可能已经有更新的消息，不能让裁判去判更晚的内容。
func judgeWindow(msgs []cache.Message, latestID string, limit int) []cache.Message {
	window := cutAtMessage(msgs, latestID)
	if len(window) == 0 {
		return nil
	}
	end := len(window)
	if limit <= 0 || limit > end {
		limit = end
	}
	return window[end-limit : end]
}

// buildJudgeText 组装裁判的 user 消息。动态内容全部在这里，system prompt 保持稳定。
//
// botID 是本群可用于 @ 比较的机器人 ID：聊天记录里的「@昵称(ID)」靠它才能分辨
// 这条消息 @ 的是机器人自己还是别的成员（同群常常还有别的机器人）。拿不到时省略。
func buildJudgeText(window []cache.Message, botName, botID, lastTurn string, hasImage, mention bool) string {
	var b strings.Builder
	if line := identityLine(botName, botID); line != "" {
		b.WriteString(line + "\n")
	}
	if lastTurn != "" {
		b.WriteString("【机器人上次发言】" + lastTurn + "\n")
	} else {
		b.WriteString("【机器人上次发言】无\n")
	}
	b.WriteString("【最近聊天】\n")
	b.WriteString(cache.BuildChatLog(window))
	if hasImage {
		b.WriteString("\n【提示】最后一条消息附带图片，图片内容已随本次请求提供。\n")
	}
	if mention {
		// 直答链路：reply 结论不用（@/私聊必须回），只取 long_form 与 focus。
		b.WriteString("【待判定】最后一条消息 @ 了机器人（或私聊），本条必须回复：reply 固定填 true，" +
			"忽略\"要不要回\"的判断，只需给出 long_form 与 focus。")
	} else {
		b.WriteString("【待判定】最近聊天里的最后一条消息。判断机器人是否应该主动回复它。")
	}
	return b.String()
}

// parseJudgeVerdict 解析裁判输出。宽松剥围栏 + 取 JSON 区间，但要求 reply 字段存在：
// 模型胡说、被中转忽略 schema 时都走"解析失败 → 不回"。
func parseJudgeVerdict(raw string) (judgeVerdict, error) {
	text := stripJSONFences(raw)
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return judgeVerdict{}, fmt.Errorf("裁判输出不是 JSON：%s", truncateJudgeLog(raw))
	}
	var verdict judgeVerdict
	if err := json.Unmarshal([]byte(text[start:end+1]), &verdict); err != nil {
		return judgeVerdict{}, fmt.Errorf("裁判输出解析失败：%w（原文 %s）", err, truncateJudgeLog(raw))
	}
	if verdict.Reply == nil {
		return judgeVerdict{}, fmt.Errorf("裁判输出缺少 reply 字段：%s", truncateJudgeLog(raw))
	}
	return verdict, nil
}

func truncateJudgeLog(raw string) string {
	runes := []rune(strings.TrimSpace(raw))
	if len(runes) <= 200 {
		return string(runes)
	}
	return string(runes[:200]) + "…"
}

// mentionsOther 这条消息是否 @ 了机器人以外的具体用户。
func mentionsOther(event onebot.Event) bool {
	botID := event.BotID()
	for _, seg := range event.Message {
		if seg.Type != onebot.SegmentMention {
			continue
		}
		if seg.String("user_id") != botID {
			return true
		}
	}
	return false
}

// isPlaceholderOnly 消息是否只有非文字内容（图片/语音/视频/文件/位置）。
// 这类消息没有文字线索，必须靠看图能力才能判断价值。
func isPlaceholderOnly(content string) bool {
	text := content
	for _, placeholder := range []string{"[图片]", "[语音]", "[视频]", "[文件]", "[位置]"} {
		text = strings.ReplaceAll(text, placeholder, "")
	}
	return strings.TrimSpace(text) == ""
}

// summarize 截断机器人发言，供裁判理解"上一句说了什么"。
func summarize(text string) string {
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= 60 {
		return string(runes)
	}
	return string(runes[:60]) + "…"
}

// humanAgo 把时间差转成中文短描述（裁判只需大致的时间感）。
func humanAgo(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "1 分钟内"
	case d < time.Hour:
		return fmt.Sprintf("%d 分钟前", int(d.Minutes()))
	default:
		return fmt.Sprintf("%d 小时前", int(d.Hours()))
	}
}
