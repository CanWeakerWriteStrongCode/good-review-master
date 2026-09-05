package cmd

import (
	"context"
	"fmt"
	"strings"

	"good-review-master/async"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/onebot"
)

// HandlerFunc 指令处理函数类型
// (event, groupID, systemPrompt, keywordPrompt, mentionerNick, extra, persona)
type HandlerFunc func(onebot.Event, string, string, string, string, string, string)

// Command 指令定义
type Command struct {
	Keyword     string
	Help        string // 仅内部指令使用
	Prompt      string // 仅用户指令使用（从 YAML 加载）
	SharedRules string
	Category    string          // "chat_review" | "internal"
	Handler     HandlerFunc
	Persona     *config.Persona // 可选：该指令的人格（渲染进 system prompt）
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
	starter    *async.Group
}

// NewRouter 创建路由器并初始化所有内部指令
func NewRouter(appCfg *config.Config, promptCfg *config.PromptConfig, llmClient llm.Client, obClient *onebot.Client, shutdownCtx context.Context) *Router {
	r := &Router{
		llmClient: llmClient,
		obClient:  obClient,
		promptCfg: promptCfg,
		appCfg:    appCfg,
		starter:   async.New(shutdownCtx),
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

	route := trieMatch(r.routeTrie, text)
	if route == nil {
		r.replyDefault(text, event, groupID)
		return
	}

	extra := strings.TrimSpace(text[len(route.Keyword):])
	systemPrompt := fmt.Sprintf("你的QQ号是 %s，昵称是 %s。\n%s",
		r.appCfg.BotQQ, r.appCfg.BotNickname, route.SharedRules)
	// 人格不放 systemPrompt 前面（会破坏聊天记录扩展缓存命中）：
	// 放到 user 消息最后，保证 [system + 聊天记录前缀] 跨指令稳定 → 换人格不失效扩展命中。
	persona := ""
	if route.Persona != nil {
		persona = RenderPersona(*route.Persona)
	}
	route.Handler(event, groupID, systemPrompt, route.Prompt, event.Nickname, extra, persona)
}

// Go 安全启动 goroutine（代理 async.Group）
func (r *Router) Go(fn func(context.Context) error) {
	r.starter.Go(fn)
}

// Wait 等待所有 goroutine 完成（代理 async.Group）
func (r *Router) Wait() error {
	return r.starter.Wait()
}

// defaultChatPrompt 未匹配指令时的提示词：不加人格，直接回应群友消息。
// 模型名来自运行时配置（config.yaml 加载后才有），故由调用方注入 modelName，
// 不能写成包级 const / 直接引用全局配置。
func (r *Router) defaultChatPrompt() string {
	return fmt.Sprintf("你知道自己是一个AI，模型是%s，请直接回应@你的群友刚才发的这条消息，自然地接着群聊。", r.appCfg.LLMConfig.ModelName)
}

// replyDefault @bot 但未匹配任何指令：直接发给大模型，不加人格。
// systemPrompt 只含机器人身份（QQ+昵称），不带指令共享规则；
// 复用 chatReview 的缓存窗口/回复/锚点逻辑，persona 传空串。
func (r *Router) replyDefault(text string, event onebot.Event, groupID string) {
	systemPrompt := fmt.Sprintf("你的QQ号是 %s，昵称是 %s。", r.appCfg.BotQQ, r.appCfg.BotNickname)
	r.chatReview(event, groupID, systemPrompt, r.defaultChatPrompt(), event.Nickname, text, "")
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
