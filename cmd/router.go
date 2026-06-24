package cmd

import (
	"fmt"
	"strings"

	"good-review-master/config"
	"good-review-master/onebot"
)

// Route 命令路由
type Route struct {
	Keyword string
	Prompt  string
	Handler func(event onebot.Event, groupID string, prompt string)
}

// handlerMap 命令名 → 处理函数
var handlerMap = map[string]func(onebot.Event, string, string){
	"chat_review": sharpTake,
	"direct_ask":  whoami,
}

// Routes 路由表（由 RebuildRoutes 动态生成）
var Routes []Route

func init() {
	RebuildRoutes()
}

// RebuildRoutes 重建路由表（系统路由 + 用户路由）
func RebuildRoutes() {
	Routes = nil

	// 系统路由（硬编码）
	Routes = append(Routes, Route{Keyword: "添加永久命令", Handler: addCommand})

	// 用户路由（从 CmdConfigs 生成）
	for cmdName, entries := range config.CmdConfigs {
		handler := handlerMap[cmdName]
		for _, entry := range entries {
			Routes = append(Routes, Route{
				Keyword: entry.Keyword,
				Prompt:  entry.Prompt,
				Handler: handler,
			})
		}
	}
}

// RouteMessage 遍历路由表匹配并分发
func RouteMessage(content string, event onebot.Event, groupID string) {
	for _, r := range Routes {
		if strings.Contains(content, r.Keyword) {
			enrichedPrompt := fmt.Sprintf("你的QQ号是 %s，昵称是 %s。\n%s", config.BotQQ, config.BotNickname, r.Prompt)
			r.Handler(event, groupID, enrichedPrompt)
			return
		}
	}
}
