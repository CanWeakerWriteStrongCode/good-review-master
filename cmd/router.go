package cmd

import (
	"fmt"
	"strings"

	"good-review-master/config"
	"good-review-master/onebot"
)

// Route 指令路由
type Route struct {
	Keyword     string
	Prompt      string
	SharedRules string
	Handler     func(event onebot.Event, groupID string, prompt string)
}

// handlerMap 指令名 → 处理函数
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

	// 系统路由（硬编码，不在 prompt_system.yaml 中）
	Routes = append(Routes,
		Route{Keyword: "添加永久指令", Handler: addCommand},
		Route{Keyword: "删除关键字", Handler: deleteCommand},
		Route{Keyword: "帮助", Handler: listCommands},
	)

	// 用户路由（从 CmdConfigs 生成）
	for cmdName, entries := range config.CmdConfigs {
		handler := handlerMap[cmdName]
		sharedRules := config.SharedRules[cmdName]
		for _, entry := range entries {
			Routes = append(Routes, Route{
				Keyword:     entry.Keyword,
				Prompt:      entry.Prompt,
				SharedRules: sharedRules,
				Handler:     handler,
			})
		}
	}
}

// RouteMessage 遍历路由表匹配并分发
func RouteMessage(content string, event onebot.Event, groupID string) {
	text := stripCQPrefix(content)
	for _, r := range Routes {
		if strings.HasPrefix(text, r.Keyword) {
			finalPrompt := r.Prompt + r.SharedRules
			enrichedPrompt := fmt.Sprintf("你的QQ号是 %s，昵称是 %s。\n%s", config.BotQQ, config.BotNickname, finalPrompt)
			r.Handler(event, groupID, enrichedPrompt)
			return
		}
	}
}

// stripCQPrefix 去除消息开头的 CQ 码和 @昵称
func stripCQPrefix(rawMsg string) string {
	s := strings.TrimSpace(rawMsg)
	// 去除 CQ at 码 [CQ:at,qq=xxx]
	if strings.HasPrefix(s, "[CQ:at,qq=") {
		if idx := strings.Index(s, "]"); idx != -1 {
			s = strings.TrimSpace(s[idx+1:])
		}
	}
	// 去除 @机器人昵称
	if config.BotNickname != "" {
		s = strings.TrimPrefix(s, "@"+config.BotNickname)
		s = strings.TrimSpace(s)
	}
	return s
}
