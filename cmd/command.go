package cmd

import (
	"good-review-master/config"
	"good-review-master/onebot"
)

// Command 指令定义
type Command struct {
	Keyword     string
	Help        string
	Category    string // "review" | "direct" | "internal"
	SharedRules string
	Handler     func(onebot.Event, string, string)
}

var registry []Command

// Register 注册内部/系统指令
func Register(cmd Command) {
	registry = append(registry, cmd)
}

// IsInternalKeyword 检查关键字是否为内部/系统指令
func IsInternalKeyword(keyword string) bool {
	for _, cmd := range registry {
		if cmd.Keyword == keyword {
			return true
		}
	}
	return false
}

// Route 路由条目
type Route struct {
	Keyword     string
	Prompt      string
	SharedRules string
	Category    string
	Handler     func(event onebot.Event, groupID string, prompt string)
}

// Routes 路由表
var Routes []Route

// handlerMap 命令类型 → 处理函数（用于用户路由）
var handlerMap = map[string]func(onebot.Event, string, string){
	"chat_review": chatReview,
}

// RebuildRoutes 重建路由表（系统路由 + 用户路由）
func RebuildRoutes() {
	Routes = nil

	// 系统路由（从 registry）
	for _, cmd := range registry {
		Routes = append(Routes, Route{
			Keyword:     cmd.Keyword,
			Prompt:      "",
			SharedRules: cmd.SharedRules,
			Category:    cmd.Category,
			Handler:     cmd.Handler,
		})
	}

	// 用户路由（从 CmdConfigs 生成）
	for cmdName, entries := range config.CmdConfigs {
		handler := handlerMap[cmdName]
		if handler == nil {
			continue
		}
		sharedRules := config.SharedRules[cmdName]
		for _, entry := range entries {
			Routes = append(Routes, Route{
				Keyword:     entry.Keyword,
				Prompt:      entry.Prompt,
				SharedRules: sharedRules,
				Category:    cmdName,
				Handler:     handler,
			})
		}
	}
}
