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

// proactiveJudgePrompt 裁判的系统提示词。
//
// **必须保持逐字节稳定**：裁判调用很频繁，固定的 system 前缀才能持续命中缓存。
// 所有动态内容（昵称、聊天记录、机器人上次发言）都放在 user 消息里。
const proactiveJudgePrompt = `你是群聊机器人的发言决策器，只判断"要不要回"，不写回复内容。
【默认沉默】拿不准就不回。

可以回（满足任一）：
1. 在跟机器人说话：叫昵称、接着机器人上一句追问；
2. 向全群求助或提问，且机器人凭知识或工具能给出实质答案；
3. 与机器人职责相关：查新闻、锐评、解释规则、回顾群历史；
4. 机器人正在参与且话题未结束，补充有明显价值；
5. 图片是新闻截图、带文字截图或图表，且与上下文相关。

不要回（命中任一）：
1. 两人私聊式对话：互相称呼、连续来回、只涉及彼此；
2. 单人自言自语、公告、吐槽，没有向任何人提问；
3. 已经有人回答或正在回答；
4. 纯社交：刷屏、接龙、斗图、表情包、梗图、争吵起哄；
5. 机器人刚说完，没人继续追问；
6. 回了也只能复读或说"不知道"；
7. 个人隐私、敏感话题。

只输出 JSON，不要代码块、不要解释：{"reply": true 或 false, "reason": "不超过 20 字的理由"}`

// proactiveVerdictSchema 裁判输出的 JSON Schema（strict 模式的硬要求：
// 所有属性都进 required、additionalProperties 必须是 false）。
var proactiveVerdictSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "reply":  {"type": "boolean", "description": "机器人是否应该主动回复最后一条消息"},
    "reason": {"type": "string",  "description": "不超过 20 字的理由"}
  },
  "required": ["reply", "reason"],
  "additionalProperties": false
}`)

// judgeVerdict 裁判结论。Reply 用指针：区分"模型明确说 false"和"字段缺失"。
type judgeVerdict struct {
	Reply  *bool  `json:"reply"`
	Reason string `json:"reason"`
}

func (v judgeVerdict) shouldReply() bool { return v.Reply != nil && *v.Reply }

// judgeNo 构造一个明确的"不回"结论（本地兜底时用，理由进日志）。
func judgeNo(reason string) judgeVerdict {
	no := false
	return judgeVerdict{Reply: &no, Reason: reason}
}

// proactiveCandidate 一个等待判定的消息（可能是运行期间被覆盖后剩下的最新一条）。
type proactiveCandidate struct {
	event   onebot.Event
	content string
}

// proactiveGroup 一个机器人在一个群里的主动插话状态。
type proactiveGroup struct {
	running     bool                // 有 worker 在跑（覆盖"判定 + 生成"两段）
	pending     *proactiveCandidate // 单槽：运行期间新消息只覆盖，不排队
	lastJudgeAt time.Time           // 上次判定开始时刻（判定间隔）
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
	// 点名（@ 本账号）必须回答，绝不经过裁判：裁判默认沉默，问它等于可能不回。
	// handler 已在入口把 @ 分给 RouteMessage，这里再兜一层，任何调用方都不能
	// 把点名消息送进"要不要回"的判定。
	if event.MentionsBot() || event.AdapterAtMe() {
		r.RouteMessage(ctx, content, event, groupID)
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

	cand := &proactiveCandidate{event: event, content: content}
	if !r.proactive.enqueue(groupID, cand, cfg, time.Now()) {
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

	verdict, err := r.judgeProactive(ctx, groupID, cand, cfg, images)
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
	logutil.Info("主动判定：回复", "group", groupID, "reason", verdict.Reason)

	systemPrompt := r.proactiveSystemPrompt(cfg, cand.event, groupID)
	reply, sent := r.chatReplyNow(ctx, cand.event, groupID, systemPrompt, "", false, images)
	if sent {
		r.proactive.recordReply(groupID, reply, time.Now())
	}
}

// proactiveSystemPrompt 主动插话的 system prompt：在普通问答基础上换成
// "主动参与"的要求（原始提示词里那句"根据最后@你的群友"在这里不成立）。
func (r *Router) proactiveSystemPrompt(cfg *config.Config, event onebot.Event, groupID string) string {
	systemPrompt := ComposeProactiveSystemPrompt(cfg, event.BotID(), "")
	if b, ok := r.getGroupPersona(groupID); ok {
		systemPrompt += "\n" + RenderPersona(b.persona, b.sharedRules)
	} else {
		systemPrompt += "\n当前无指定人格，现在是普通大模型问答模式：请如实正常回答，不要代入任何虚构人格或角色。"
	}
	return systemPrompt
}

// judgeProactive 问裁判"这条要不要回"。任何失败都由调用方按"不回"处理。
// images 是已经下载好的候选图片（handleProactive 只下载一次，生成阶段复用）。
func (r *Router) judgeProactive(ctx context.Context, groupID string, cand *proactiveCandidate, cfg *config.Config, images []llm.Image) (judgeVerdict, error) {
	msgs := r.cacheStore().GetGroupCache(groupID, cfg.MaxCacheMsg).GetAll()
	window := judgeWindow(msgs, cand.event.MessageID, cfg.ProactiveWindow)
	if len(window) == 0 {
		return judgeVerdict{}, fmt.Errorf("没有可判定的聊天记录")
	}

	// 纯图片消息但一张都没读到：裁判看不到内容，只能按"不回"处理
	if isPlaceholderOnly(cand.content) && len(images) == 0 {
		return judgeNo("纯图片消息未能读取到图片"), nil
	}

	dataURLs := make([]string, 0, len(images))
	for _, img := range images {
		dataURLs = append(dataURLs, llm.DataURL(img.MIMEType, img.Data))
	}
	userText := buildJudgeText(window, cfg.BotNickname, r.proactive.lastTurn(groupID, time.Now()), len(dataURLs) > 0)
	messages := []llm.Message{
		{Role: llm.RoleSystem, Content: proactiveJudgePrompt},
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
// 以候选消息为右边界：worker 判定时缓存里可能已经有更新的消息，不能让裁判去判更晚的内容。
func judgeWindow(msgs []cache.Message, latestID string, limit int) []cache.Message {
	if len(msgs) == 0 {
		return nil
	}
	end := len(msgs)
	if latestID != "" {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].MessageID == latestID {
				end = i + 1
				break
			}
		}
	}
	if limit <= 0 || limit > end {
		limit = end
	}
	return msgs[end-limit : end]
}

// buildJudgeText 组装裁判的 user 消息。动态内容全部在这里，system prompt 保持稳定。
func buildJudgeText(window []cache.Message, botName, lastTurn string, hasImage bool) string {
	var b strings.Builder
	if botName != "" {
		b.WriteString("【机器人】昵称：" + botName + "\n")
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
	b.WriteString("【待判定】最近聊天里的最后一条消息。判断机器人是否应该主动回复它。")
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
