package router

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"good-review-master/async"
	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/telemetry"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// HandlerFunc 指令处理函数类型
// (ctx, event, groupID, systemPrompt, keywordPrompt, mentionerNick, extra)
// systemPrompt 已含渲染好的人格块（若该路由有自带人格）；extra 是关键字后的补充文本。
//
// ctx 是分发链路的上下文：它承载 trace span，并且随应用关闭而取消。
// 处理器里发起的所有出站调用（大模型、MCP）都该挂在它下面——
// 既能让一次"消息 → 路由 → 大模型 → 工具"成为一条完整链路，
// 也能让 Ctrl+C 取消掉正在跑的那次调用。
type HandlerFunc func(context.Context, onebot.Event, string, string, string, string, string)

// Command 指令定义
type Command struct {
	Keyword     string
	Help        string // 仅内部指令使用
	Prompt      string // 仅用户指令使用（从 YAML 加载）
	SharedRules string
	Category    string // "chat_review" | "internal"
	Handler     HandlerFunc
	Persona     *config.Persona // 可选：该指令的人格（渲染进 system prompt）
}

// groupPersonaBinding 某群"当前人格"的快照：切换时从路由表里把源关键字的人格
// 与共享规则一起值拷贝进来（config 重载/rebuild 会让旧 *Command/*Persona 指针失效，
// 存值拷贝才能自包含）。纯 @ 聊天（replyDefault）据此渲染人格块。
type groupPersonaBinding struct {
	keyword     string
	persona     config.Persona
	sharedRules string
}

// trieNode 前缀树节点
type trieNode struct {
	children map[rune]*trieNode
	route    *Command // 到达该节点时的匹配路由（nil 表示非终点）
}

// trieInsert 向前缀树中插入路由
func trieInsert(root *trieNode, keyword string, rt *Command) {
	node := root
	for _, ch := range keyword {
		if node.children[ch] == nil {
			node.children[ch] = &trieNode{children: make(map[rune]*trieNode)}
		}
		node = node.children[ch]
	}
	node.route = rt
}

// trieMatch 前缀匹配，返回最长匹配路由（O(k)，与路由总数无关）
func trieMatch(root *trieNode, text string) *Command {
	node := root
	var lastMatch *Command
	for _, ch := range text {
		next, ok := node.children[ch]
		if !ok {
			break
		}
		node = next
		if node.route != nil {
			lastMatch = node.route
		}
	}
	return lastMatch
}

// routeTable 路由表的一份完整快照（发布后不可变）。
//
// 前缀树与列表放进**同一个**结构体整体替换，而不是各自一个 atomic：
// 分开的话，分发拿到新 trie、而「帮助」列出旧 routes 是可能的，
// 于是"列出来的指令"与"实际能命中的指令"对不上——这类不一致极难排查。
type routeTable struct {
	trie   *trieNode
	routes []Command // 全部指令（内部 + 用户），内部指令 Category="internal"
}

// Router 指令路由器
type Router struct {
	// table 是当前路由表，rebuild 时整体替换、分发时无锁读取。
	// 热重载让 rebuild 可能来自 informer goroutine，同时消息分发与内部指令的
	// 异步任务都在读它——必须原子替换，不能就地改。
	table atomic.Pointer[routeTable]

	// internalCommands 内部指令，只在启动时 register，之后只读。
	// 单独存一份而不是从 table.routes 里筛 Category=="internal"：
	// 那种做法要求"内部指令一定还在 routes 里"，是个隐式约定；
	// 分开存之后 rebuild 的输入是明确的，也不会随 table 的替换而丢失。
	internalCommands []Command

	handlerMap map[string]HandlerFunc
	llmClient  llm.Client
	obClient   *onebot.Client
	promptCfg  *config.PromptConfig
	appCfg     config.Snapshot
	mcp        MCPProvider // MCP 工具提供者，未启用时为 nil
	starter    *async.Group

	// store 是本工作空间的缓存与锚点。默认指向进程级默认 Store（单账号测试、
	// 旧调用点）；多工作空间部署由 workspace 包注入各自的 Store。
	store *cache.Store
	// workspace 是工作空间 ID，只用于指标与日志标签（每个 Router 一个工作空间）。
	workspace string

	// groupPersona 各群当前人格（#切换人格/#取消人格 维护，纯 @ 聊天时生效）。
	// 同一分发路径读写，加锁防未来并发扩展。
	groupPersonaMu sync.RWMutex
	groupPersona   map[string]*groupPersonaBinding // groupID → 本群当前人格

	// proactive 主动插话的每群状态。键是 groupID，但因为 Router 每个工作空间一个，
	// 实际语义是 (workspace, group) 联合——同一个群里的两个机器人互不阻塞。
	proactive proactiveRegistry

	// personaStore 可选：把群人格的**源关键字**持久化到共享 state.db（带 workspace_id）。
	// 只存关键字而不是渲染后的文本：提示词热更新后要按新版本重新绑定。
	personaStore PersonaStore

	// displayName 按群查成员显示名（工作空间的成员目录）。nil 时日志与 handler
	// 只用 user_id，不影响功能——昵称只是给人看的。
	displayName func(groupID, userID string) string
}

// PersonaStore 持久化群当前人格。由工作空间的 state.Store 实现。
type PersonaStore interface {
	SaveGroupPersona(groupID, keyword string)
	DeleteGroupPersona(groupID string)
}

// NewRouter 创建路由器并初始化所有内部指令。
// mcpProvider 可传 nil（MCP 未启用），此时对话一律走单轮无工具调用。
func NewRouter(appCfg config.Snapshot, promptCfg *config.PromptConfig, llmClient llm.Client, obClient *onebot.Client, mcpProvider MCPProvider, shutdownCtx context.Context) *Router {
	r := &Router{
		llmClient:    llmClient,
		obClient:     obClient,
		promptCfg:    promptCfg,
		appCfg:       appCfg,
		mcp:          mcpProvider,
		store:        cache.DefaultStore(),
		starter:      async.New(shutdownCtx),
		groupPersona: make(map[string]*groupPersonaBinding),
	}
	r.handlerMap = map[string]HandlerFunc{
		"chat_review": r.chatReview,
	}
	r.registerInternalCommands()
	r.Rebuild()
	return r
}

// SetStore 注入本工作空间的缓存与锚点。必须在开始处理消息前调用。
func (r *Router) SetStore(store *cache.Store) {
	if store != nil {
		r.store = store
	}
}

// SetWorkspace 设置工作空间 ID，只影响指标与日志标签。
func (r *Router) SetWorkspace(id string) { r.workspace = id }

// storeOr 返回注入的 Store；未注入时退回进程级默认 Store。
func (r *Router) cacheStore() *cache.Store {
	if r.store == nil {
		return cache.DefaultStore()
	}
	return r.store
}

// WorkspaceID 返回本 Router 所属工作空间（测试与面板用）。
func (r *Router) WorkspaceID() string { return r.workspace }

// register 注册内部/系统指令（仅启动期调用，之后 internalCommands 只读）
func (r *Router) register(rt Command) {
	r.internalCommands = append(r.internalCommands, rt)
}

// isInternalKeyword 检查关键字是否为内部/系统指令。
// 直接查 internalCommands：内部指令不随热重载变化，没必要为它去读路由表快照。
func (r *Router) isInternalKeyword(keyword string) bool {
	for _, cmd := range r.internalCommands {
		if cmd.Category == "internal" && cmd.Keyword == keyword {
			return true
		}
	}
	return false
}

// Rebuild 重建路由表（前缀树匹配 + 列表展示）。
//
// 导出版本供配置热更新调用：提示词文件变了之后，光 Reload 数据还不够，
// 路由表是从数据派生的，必须跟着重建才能让新关键字真正可命中。
//
// 无锁也无所谓并发：它只构造一份全新的 table 再原子替换，
// 两个并发调用各建一份、后者胜出，结果完全一致（输入相同）；
// 读者要么看到旧表要么看到新表，不会看到半张。
func (r *Router) Rebuild() {
	table := &routeTable{
		trie:   &trieNode{children: make(map[rune]*trieNode)},
		routes: make([]Command, 0, len(r.internalCommands)),
	}

	// 内部指令始终在（不随提示词文件变化）
	for i := range r.internalCommands {
		rt := &r.internalCommands[i]
		table.routes = append(table.routes, *rt)
		trieInsert(table.trie, rt.Keyword, rt)
	}

	// 用户指令：从提示词快照生成。
	// 整块重建都基于**同一份**快照，不会一半用旧数据一半用新数据。
	promptData := r.promptCfg.Snapshot()
	for cmdName, entries := range promptData.CmdConfigs {
		handler := r.handlerMap[cmdName]
		if handler == nil {
			continue
		}
		sharedRules := promptData.SharedRules[cmdName]
		for _, entry := range entries {
			rt := Command{
				Keyword:     entry.Keyword,
				Prompt:      entry.Prompt,
				SharedRules: sharedRules,
				Category:    cmdName,
				Handler:     handler,
				Persona:     entry.Persona,
			}
			table.routes = append(table.routes, rt)
			trieInsert(table.trie, entry.Keyword, &rt)
		}
	}

	r.table.Store(table)
}

// routesSnapshot 返回当前路由表的指令列表（供「帮助」等只读遍历）。
func (r *Router) routesSnapshot() []Command {
	return r.table.Load().routes
}

// RouteMessage 前缀树匹配并分发。
//
// ctx 由调用方传入（轮询循环的应用 ctx，或 debug trigger 的 HTTP 请求 ctx），
// 一路传到处理器里的大模型与 MCP 调用——这条链是链路追踪的主干。
func (r *Router) RouteMessage(ctx context.Context, content string, event onebot.Event, groupID string) {
	// 路由本身也记一个 span：它是"一条群消息"与"一次大模型调用"之间的那一跳，
	// 少了它，链路图上就看不出是哪条消息触发的、命中了哪个关键字。
	ctx, span := telemetry.StartSpan(ctx, "router.route",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(attribute.String("messaging.group_id", groupID)))
	defer span.End()

	// 开头取一次快照、全程共用：拼 systemPrompt 要用到模型名/昵称/看图开关，
	// 它们必须自洽。若每处都调 r.appCfg()，热更新正好插在中间就会拼出
	// "新模型名 + 旧昵称"这种混合提示词。
	cfg := r.appCfg()

	text := r.stripCQPrefix(content, cfg)
	if text == "" {
		return
	}

	route := trieMatch(r.table.Load().trie, text)
	if route == nil {
		// 未命中：走普通问答（replyDefault 会视群人格再补一段）
		// 焦点提示由 replyDefault 在人格块之后追加，保证它是最后一条指令。
		r.replyDefault(ctx, text, event, groupID, ComposeSystemPrompt(cfg, event.BotID(), "", r.promptCfg.ReplyRequirement(), ""))
		return
	}

	extra := strings.TrimSpace(text[len(route.Keyword):])
	// 关键字路由只把"路由自带人格"渲染进 systemPrompt 末尾；
	// 群人格（#切换人格）只在未命中关键字的纯 @ 聊天里生效，见 replyDefault。
	persona := ""
	if route.Persona != nil {
		persona = RenderPersona(*route.Persona, route.SharedRules, r.promptCfg.PersonaDirective())
	}
	// 有人直接 @ 机器人说话：默认聚焦最后一条，避免把回复铺到更早的话题上。
	systemPrompt := ComposeSystemPrompt(cfg, event.BotID(), persona, r.promptCfg.ReplyRequirement(), r.promptCfg.FocusPrompt(true))
	span.SetAttributes(attribute.String("router.keyword", route.Keyword),
		attribute.String("router.category", route.Category))
	route.Handler(ctx, event, groupID, systemPrompt, route.Prompt, r.nameOf(groupID, event.UserID), extra)
}

// reply 回复触发消息：引用这条触发消息本身，并 @ 发送者。
// 引用的是 event.MessageID（别人 @ 我的那条），不是 event.QuotedMessageID()
// （那条是发送者自己引用的消息）。QQ 官方用 message_reference + at 段；
// 拿不到该消息的 REFIDX 时插件会退化成只 @ 不引用。
//
// 私聊没有引用/@ 语义，直接按 user_id 发文本。
// 被点名回复同时清空主动插话的"连续发言"计数：对话对象已经切回机器人。
func (r *Router) reply(event onebot.Event, chatID, text string) {
	if event.IsPrivate() {
		r.obClient.SendPrivateReply(event.UserID, text)
	} else {
		r.obClient.SendGroupReply(chatID, event.MessageID, event.UserID, text)
	}
	r.proactive.noteDirect(chatID, text, time.Now())
}

// replyQuoted 引用触发消息、不 @ 人（主动插话用）。
// 主动接话不该把某人"架"出来回答，引用只用来让群友知道在接哪一句。
func (r *Router) replyQuoted(event onebot.Event, chatID, text string) {
	if event.IsPrivate() {
		r.obClient.SendPrivateReply(event.UserID, text)
		return
	}
	r.obClient.SendGroupReply(chatID, event.MessageID, "", text)
}

// ComposeSystemPrompt 拼装发给大模型的 system prompt：
// 机器人身份（模型名 / 昵称 / 稳定 ID）+ 聊天记录约定 +（启用看图时的图片说明）+ 可选的人格块
// + 末尾的回复焦点提示。
// botID 为空时省略 ID 行（例如离线评测或拿不到 bot 身份时）。
// requirement 是【要求】句，focus 是回复焦点提示（空串表示不追加），@ 回复分别传
// promptCfg.ReplyRequirement() / promptCfg.FocusPrompt(true)。
//
// 导出是给 tests/eval 用的：离线评测必须拿到与线上**逐字相同**的提示词。
// 评测集若自己再拼一份，防的就是"评测全绿但线上照样回归"这种最没用的结果——
// 而 5 处字符串拼接正是最容易各改各的那种代码。
func ComposeSystemPrompt(cfg *config.Config, botID, personaBlock, requirement, focus string) string {
	systemPrompt := composeSystemPrompt(cfg, botID, personaBlock, requirement)
	if focus != "" {
		systemPrompt += "\n" + focus
	}
	return systemPrompt
}

// ComposeProactiveSystemPrompt 主动插话变体：聊天记录约定、身份、人格块都与上面一致，
// 只把"【要求】"那句换成主动参与的说法（由 prompt_system.yaml 的
// internal.proactive_requirement 提供）——那条消息并没有 @ 机器人，
// 继续沿用"按最后的消息回复/执行指令"会让模型以为自己被点名。
func ComposeProactiveSystemPrompt(cfg *config.Config, botID, personaBlock, requirement string) string {
	return composeSystemPrompt(cfg, botID, personaBlock, requirement)
}

func composeSystemPrompt(cfg *config.Config, botID, personaBlock, requirement string) string {
	identity := ""
	if cfg.BotNickname != "" {
		identity = fmt.Sprintf("你的昵称是【%s】", cfg.BotNickname)
	}
	if botID != "" {
		if identity != "" {
			identity += "，"
		}
		identity += fmt.Sprintf("用户ID是【%s】", botID)
	}
	if identity != "" {
		identity += "。"
	}
	systemPrompt := fmt.Sprintf("你是一个AI，模型是%s。【工具使用】当用户询问真实信息时，应调用对应MCP工具，禁止自行编造答案。"+
		"%s【聊天记录】每行一条群消息，message_id 是本窗口内的消息序号，user_id 是发言者的稳定ID，nickname 是昵称，message 是正文；"+
		"@昵称(ID) 表示 @ 了该用户，昵称可能变化，以括号里的 ID 为准。"+
		"reply_id 表示该消息引用了哪条 message_id。"+
		requirement,
		cfg.LLMConfig.ModelName, identity)
	// agent「看图」：提示模型需要看清图片时用 view_image 按需查看（静态指令，前缀稳定）
	if cfg.LLMConfig.ImageMax > 0 {
		systemPrompt += "\n【图片】群消息里的图片默认不随文字附上；需要看图时，按候选列表里的对应 url 调用 view_image 工具（实际查看有数量上限，达到后请直接作答）。不要编造没实际看过的图片内容。"
	}
	if personaBlock != "" {
		systemPrompt += "\n" + personaBlock
	}
	return systemPrompt
}

// Go 安全启动 goroutine（代理 async.Group）
func (r *Router) Go(fn func(context.Context) error) {
	r.starter.Go(fn)
}

// GoCtx 在调用方 ctx 基础上安全启动 goroutine（代理 async.Group.GoCtx）。
// 分发链路上的异步任务用这个，才能把 trace span 带进 goroutine。
func (r *Router) GoCtx(parent context.Context, fn func(context.Context) error) {
	r.starter.GoCtx(parent, fn)
}

// Wait 等待所有 goroutine 完成（代理 async.Group）
func (r *Router) Wait() error {
	return r.starter.Wait()
}

// getGroupPersona 返回某群当前人格快照（#切换人格 设置）
func (r *Router) getGroupPersona(groupID string) (*groupPersonaBinding, bool) {
	r.groupPersonaMu.RLock()
	defer r.groupPersonaMu.RUnlock()
	b, ok := r.groupPersona[groupID]
	return b, ok
}

// setGroupPersona 记录某群当前人格
func (r *Router) setGroupPersona(groupID string, b *groupPersonaBinding) {
	r.groupPersonaMu.Lock()
	r.groupPersona[groupID] = b
	r.groupPersonaMu.Unlock()
	if r.personaStore != nil && b != nil {
		r.personaStore.SaveGroupPersona(groupID, b.keyword)
	}
}

// clearGroupPersona 清空某群当前人格（#取消人格）
func (r *Router) clearGroupPersona(groupID string) {
	r.groupPersonaMu.Lock()
	delete(r.groupPersona, groupID)
	r.groupPersonaMu.Unlock()
	if r.personaStore != nil {
		r.personaStore.DeleteGroupPersona(groupID)
	}
}

// SetDisplayName 注入成员显示名查询（工作空间成员目录）。必须在分发前调用。
func (r *Router) SetDisplayName(fn func(groupID, userID string) string) { r.displayName = fn }

// nameOf 返回成员显示名；没有目录时返回空串，调用方自行回退。
func (r *Router) nameOf(groupID, userID string) string {
	if r.displayName == nil {
		return ""
	}
	return r.displayName(groupID, userID)
}

// SetPersonaStore 注入人格持久化。必须在开始处理消息前调用。
func (r *Router) SetPersonaStore(store PersonaStore) { r.personaStore = store }

// RestorePersonas 把 state.db 里本工作空间存的关键字重新绑定成当前路由表里的人格。
// 关键字在提示词里已经不存在时降级为默认人格并告警——提示词被人改过是常态。
func (r *Router) RestorePersonas(stored map[string]string) {
	for groupID, keyword := range stored {
		binding := r.personaBinding(keyword)
		if binding == nil {
			logutil.Warn("持久化的群人格关键字已不存在，按默认人格处理", "group", groupID, "keyword", keyword)
			if r.personaStore != nil {
				r.personaStore.DeleteGroupPersona(groupID)
			}
			continue
		}
		r.setGroupPersona(groupID, binding)
	}
}

// personaBinding 按关键字找一条自带人格的路由，复制成自包含绑定。
func (r *Router) personaBinding(keyword string) *groupPersonaBinding {
	for _, route := range r.routesSnapshot() {
		if route.Keyword == keyword && route.Persona != nil {
			return &groupPersonaBinding{
				keyword:     keyword,
				persona:     *route.Persona,
				sharedRules: route.SharedRules,
			}
		}
	}
	return nil
}

// availablePersonaNames 列出所有"自带人格"的关键字（可作为 #切换人格 的目标人格）。
// 直接遍历 rebuild 好的路由表：新增/删除关键字经 Reload()+rebuild() 后自动同步。
func (r *Router) availablePersonaNames() []string {
	var names []string
	for _, route := range r.routesSnapshot() {
		if route.Persona != nil {
			names = append(names, route.Keyword)
		}
	}
	return names
}

// replyDefault @bot 但未匹配任何指令：直接发给大模型。
// systemPrompt 只含机器人身份（模型名+昵称），不带指令共享规则；若本群已 #切换人格，
// 则在末尾追加渲染好的人格块（仅纯 @ 聊天生效）；否则显式声明当前是普通问答、
// 不代入任何人格，避免模型在曾被切换过人格的群里继续沿用旧人设。
// 复用 chatReview 的缓存窗口/回复/锚点逻辑。
func (r *Router) replyDefault(ctx context.Context, text string, event onebot.Event, groupID string, systemPrompt string) {
	if b, ok := r.getGroupPersona(groupID); ok {
		systemPrompt += "\n" + RenderPersona(b.persona, b.sharedRules, r.promptCfg.PersonaDirective())
	} else {
		systemPrompt += "\n当前无指定人格，现在是普通大模型问答模式：请如实正常回答，不要代入任何虚构人格或角色。"
	}
	systemPrompt += "\n" + r.promptCfg.FocusPrompt(true)
	r.chatReview(ctx, event, groupID, systemPrompt, "", r.nameOf(groupID, event.UserID), text)
}

// stripCQPrefix 去除消息开头的 @ 标记，让后面的文本能命中关键字。
// 支持「@昵称(ID)」与手打的「@昵称」两种写法。
// 只用于路由匹配；聊天记录里的完整内容不会被改动。
func (r *Router) stripCQPrefix(rawMsg string, cfg *config.Config) string {
	text := strings.TrimSpace(rawMsg)
	// 每轮只剥一个标记，剥完再看下一个；一条消息可能先后 @ 了多个人，机器人不一定排第一
	for {
		before := text
		if trimmed := trimLeadingMentionToken(text); trimmed != text {
			text = trimmed
		} else if cfg.BotNickname != "" && strings.HasPrefix(text, "@"+cfg.BotNickname) {
			text = strings.TrimPrefix(text, "@"+cfg.BotNickname)
		}
		text = strings.TrimSpace(text)
		if text == before {
			break
		}
	}
	return text
}

// trimLeadingMentionToken 去掉开头的「@昵称(ID)」；没有 ID 括号时原样返回。
func trimLeadingMentionToken(text string) string {
	if !strings.HasPrefix(text, "@") {
		return text
	}
	open := strings.IndexByte(text, '(')
	if open < 0 {
		return text
	}
	closeIdx := strings.IndexByte(text[open+1:], ')')
	if closeIdx < 0 {
		return text
	}
	return text[open+1+closeIdx+1:]
}
