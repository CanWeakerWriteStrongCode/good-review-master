package speech

import (
	"context"

	"good-review-master/config"
	"good-review-master/onebot"
)

// 指令表：关键字 → 处理器。两条来源合成一张表——
// 内部指令（register 时定下，不随热重载变）与用户指令（从提示词快照派生）。
// 表整体构造好再原子替换，分发路径上不加锁。

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
