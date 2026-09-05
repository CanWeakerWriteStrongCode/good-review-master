package cmd

import (
	"context"
	"fmt"
	"strings"
	"sync"

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

// Router 指令路由器
type Router struct {
	routeTrie  *trieNode
	routes     []Command // 全部指令（内部 + 用户），内部指令 Category="internal"
	handlerMap map[string]HandlerFunc
	llmClient  llm.Client
	obClient   *onebot.Client
	promptCfg  *config.PromptConfig
	appCfg     *config.Config
	mcp        MCPProvider // MCP 工具提供者，未启用时为 nil
	starter    *async.Group

	// groupPersona 各群当前人格（#切换人格/#取消人格 维护，纯 @ 聊天时生效）。
	// 内存态，重启清空，与 cache/锚点一致。同一分发路径读写，加锁防未来并发扩展。
	groupPersonaMu sync.RWMutex
	groupPersona   map[string]*groupPersonaBinding // groupID → 本群当前人格
}

// NewRouter 创建路由器并初始化所有内部指令。
// mcpProvider 可传 nil（MCP 未启用），此时对话一律走单轮无工具调用。
func NewRouter(appCfg *config.Config, promptCfg *config.PromptConfig, llmClient llm.Client, obClient *onebot.Client, mcpProvider MCPProvider, shutdownCtx context.Context) *Router {
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
	r.rebuild()
	return r
}

// register 注册内部/系统指令
func (r *Router) register(rt Command) {
	r.routes = append(r.routes, rt)
}

// isInternalKeyword 检查关键字是否为内部/系统指令
func (r *Router) isInternalKeyword(keyword string) bool {
	for _, cmd := range r.routes {
		if cmd.Category == "internal" && cmd.Keyword == keyword {
			return true
		}
	}
	return false
}

// rebuild 重建路由表（前缀树匹配 + 列表展示）
func (r *Router) rebuild() {
	// 保存内部指令（Category="internal"），重建后重新插入
	var internal []Command
	for _, rt := range r.routes {
		if rt.Category == "internal" {
			internal = append(internal, rt)
		}
	}

	r.routes = nil
	r.routeTrie = &trieNode{children: make(map[rune]*trieNode)}

	// 系统路由（内部指令）
	for i := range internal {
		rt := &internal[i]
		r.routes = append(r.routes, *rt)
		trieInsert(r.routeTrie, rt.Keyword, rt)
	}

	// 用户路由（从 CmdConfigs 生成）
	for cmdName, entries := range r.promptCfg.CmdConfigs {
		handler := r.handlerMap[cmdName]
		if handler == nil {
			continue
		}
		sharedRules := r.promptCfg.SharedRules[cmdName]
		for _, entry := range entries {
			rt := Command{
				Keyword:     entry.Keyword,
				Prompt:      entry.Prompt,
				SharedRules: sharedRules,
				Category:    cmdName,
				Handler:     handler,
				Persona:     entry.Persona,
			}
			r.routes = append(r.routes, rt)
			trieInsert(r.routeTrie, entry.Keyword, &rt)
		}
	}
}

// RouteMessage 前缀树匹配并分发
func (r *Router) RouteMessage(content string, event onebot.Event, groupID string) {
	text := r.stripCQPrefix(content)
	if text == "" {
		return
	}

	systemPrompt := fmt.Sprintf("你是一个AI，模型是%s。【工具使用】当用户询问真实信息时，应调用对应MCP工具，禁止自行编造答案。"+
		"你的QQ号是【%s】，昵称是【%s】。【要求】发给你的内容是聊天记录，根据最后@你的群友发的消息，继续聊天或者执行指令后回复。", r.appCfg.LLMConfig.ModelName, r.appCfg.BotQQ, r.appCfg.BotNickname)
	route := trieMatch(r.routeTrie, text)
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
	for _, route := range r.routes {
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

// stripCQPrefix 去除消息开头的 CQ 码和 @昵称
func (r *Router) stripCQPrefix(rawMsg string) string {
	text := strings.TrimSpace(rawMsg)
	// 去除 CQ at 码 [CQ:at,qq=xxx]
	if strings.HasPrefix(text, "[CQ:at,qq=") {
		if idx := strings.Index(text, "]"); idx != -1 {
			text = strings.TrimSpace(text[idx+1:])
		}
	}
	// 去除 @机器人昵称
	if r.appCfg.BotNickname != "" {
		text = strings.TrimPrefix(text, "@"+r.appCfg.BotNickname)
		text = strings.TrimSpace(text)
	}
	return text
}
