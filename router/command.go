package router

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"good-review-master/async"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/onebot"
)

// HandlerFunc 指令处理函数类型
// (event, groupID, systemPrompt, keywordPrompt, mentionerNick, extra)
// systemPrompt 已含渲染好的人格块（若该路由有自带人格）；extra 是关键字后的补充文本。
type HandlerFunc func(onebot.Event, string, string, string, string, string)

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

	// groupPersona 各群当前人格（#切换人格/#取消人格 维护，纯 @ 聊天时生效）。
	// 内存态，重启清空，与 cache/锚点一致。同一分发路径读写，加锁防未来并发扩展。
	groupPersonaMu sync.RWMutex
	groupPersona   map[string]*groupPersonaBinding // groupID → 本群当前人格
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

// RouteMessage 前缀树匹配并分发
func (r *Router) RouteMessage(content string, event onebot.Event, groupID string) {
	// 开头取一次快照、全程共用：拼 systemPrompt 要用到模型名/QQ/昵称/看图开关，
	// 它们必须自洽。若每处都调 r.appCfg()，热更新正好插在中间就会拼出
	// "新模型名 + 旧昵称"这种混合提示词。
	cfg := r.appCfg()

	text := r.stripCQPrefix(content, cfg)
	if text == "" {
		return
	}

	systemPrompt := fmt.Sprintf("你是一个AI，模型是%s。【工具使用】当用户询问真实信息时，应调用对应MCP工具，禁止自行编造答案。"+
		"你的QQ号是【%s】，昵称是【%s】。【要求】发给你的内容是聊天记录，根据最后@你的群友发的消息，继续聊天或者执行指令后回复。", cfg.LLMConfig.ModelName, cfg.BotQQ, cfg.BotNickname)
	// agent「看图」：提示模型需要看清图片时用 view_image 按需查看（静态指令，前缀稳定）
	if cfg.LLMConfig.ImageMax > 0 {
		systemPrompt += "\n【图片】群消息里的图片默认不随文字附上；需要看图时，按候选列表里的对应 url 调用 view_image 工具（实际查看有数量上限，达到后请直接作答）。不要编造没实际看过的图片内容。"
	}
	route := trieMatch(r.table.Load().trie, text)
	if route == nil {
		r.replyDefault(text, event, groupID, systemPrompt)
		return
	}

	extra := strings.TrimSpace(text[len(route.Keyword):])
	// 关键字路由只把"路由自带人格"渲染进 systemPrompt 末尾；
	// 群人格（#切换人格）只在未命中关键字的纯 @ 聊天里生效，见 replyDefault。
	persona := ""
	if route.Persona != nil {
		persona = RenderPersona(*route.Persona, route.SharedRules)
	}
	systemPrompt += "\n" + persona
	route.Handler(event, groupID, systemPrompt, route.Prompt, event.Nickname, extra)
}

// Go 安全启动 goroutine（代理 async.Group）
func (r *Router) Go(fn func(context.Context) error) {
	r.starter.Go(fn)
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
	defer r.groupPersonaMu.Unlock()
	r.groupPersona[groupID] = b
}

// clearGroupPersona 清空某群当前人格（#取消人格）
func (r *Router) clearGroupPersona(groupID string) {
	r.groupPersonaMu.Lock()
	defer r.groupPersonaMu.Unlock()
	delete(r.groupPersona, groupID)
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
// systemPrompt 只含机器人身份（QQ+昵称），不带指令共享规则；若本群已 #切换人格，
// 则在末尾追加渲染好的人格块（仅纯 @ 聊天生效）；否则显式声明当前是普通问答、
// 不代入任何人格，避免模型在曾被切换过人格的群里继续沿用旧人设。
// 复用 chatReview 的缓存窗口/回复/锚点逻辑。
func (r *Router) replyDefault(text string, event onebot.Event, groupID string, systemPrompt string) {
	if b, ok := r.getGroupPersona(groupID); ok {
		systemPrompt += "\n" + RenderPersona(b.persona, b.sharedRules)
	} else {
		systemPrompt += "\n当前无指定人格，现在是普通大模型问答模式：请如实正常回答，不要代入任何虚构人格或角色。"
	}
	r.chatReview(event, groupID, systemPrompt, "", event.Nickname, text)
}

// stripCQPrefix 去除消息开头的 CQ 码和 @昵称。
// cfg 由调用方传入（就是它开头取的那一份），避免"清洗用旧昵称、拼提示词用新昵称"。
func (r *Router) stripCQPrefix(rawMsg string, cfg *config.Config) string {
	text := strings.TrimSpace(rawMsg)
	// 去除 CQ at 码 [CQ:at,qq=xxx]
	if strings.HasPrefix(text, "[CQ:at,qq=") {
		if idx := strings.Index(text, "]"); idx != -1 {
			text = strings.TrimSpace(text[idx+1:])
		}
	}
	// 去除 @机器人昵称
	if cfg.BotNickname != "" {
		text = strings.TrimPrefix(text, "@"+cfg.BotNickname)
		text = strings.TrimSpace(text)
	}
	return text
}
