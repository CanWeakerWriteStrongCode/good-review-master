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
type HandlerFunc func(onebot.Event, string, string)

// Command 指令定义
type Command struct {
	Keyword     string
	Help        string
	Category    string // "chat_review" | "direct_ask" | "internal"
	SharedRules string
	Handler     HandlerFunc
}

// Route 路由条目
type Route struct {
	Keyword     string
	Prompt      string
	SharedRules string
	Category    string
	Handler     HandlerFunc
}

// trieNode 前缀树节点
type trieNode struct {
	children map[rune]*trieNode
	route    *Route // 到达该节点时的匹配路由（nil 表示非终点）
}

// trieInsert 向前缀树中插入路由
func trieInsert(root *trieNode, keyword string, rt *Route) {
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
func trieMatch(root *trieNode, text string) *Route {
	node := root
	var lastMatch *Route
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
	routes     []Route // 用于帮助列表遍历
	registry   []Command
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
func (r *Router) register(cmd Command) {
	r.registry = append(r.registry, cmd)
}

// isInternalKeyword 检查关键字是否为内部/系统指令
func (r *Router) isInternalKeyword(keyword string) bool {
	for _, cmd := range r.registry {
		if cmd.Keyword == keyword {
			return true
		}
	}
	return false
}

// rebuild 重建路由表（前缀树匹配 + 列表展示）
func (r *Router) rebuild() {
	r.routes = nil
	r.routeTrie = &trieNode{children: make(map[rune]*trieNode)}

	// 系统路由（从 registry）
	for _, cmd := range r.registry {
		rt := Route{
			Keyword:     cmd.Keyword,
			Prompt:      "",
			SharedRules: cmd.SharedRules,
			Category:    cmd.Category,
			Handler:     cmd.Handler,
		}
		r.routes = append(r.routes, rt)
		trieInsert(r.routeTrie, cmd.Keyword, &rt)
	}

	// 用户路由（从 CmdConfigs 生成）
	for cmdName, entries := range r.promptCfg.CmdConfigs {
		handler := r.handlerMap[cmdName]
		if handler == nil {
			continue
		}
		sharedRules := r.promptCfg.SharedRules[cmdName]
		for _, entry := range entries {
			rt := Route{
				Keyword:     entry.Keyword,
				Prompt:      entry.Prompt,
				SharedRules: sharedRules,
				Category:    cmdName,
				Handler:     handler,
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
		return
	}

	extra := strings.TrimSpace(text[len(route.Keyword):])
	finalPrompt := route.Prompt + route.SharedRules
	if extra != "" {
		finalPrompt += "\n用户补充,优先级很高:" + extra
	}
	enrichedPrompt := fmt.Sprintf("你的QQ号是 %s，昵称是 %s。当前@你的是群友 %s。\n%s",
		r.appCfg.BotQQ, r.appCfg.BotNickname, event.Nickname, finalPrompt)
	route.Handler(event, groupID, enrichedPrompt)
}

// Go 安全启动 goroutine（代理 async.Group）
func (r *Router) Go(fn func(context.Context) error) {
	r.starter.Go(fn)
}

// Wait 等待所有 goroutine 完成（代理 async.Group）
func (r *Router) Wait() error {
	return r.starter.Wait()
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
