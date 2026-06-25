package cmd

import (
	"fmt"
	"strings"

	"good-review-master/config"
	"good-review-master/onebot"
)

func init() {
	RebuildRoutes()
}

// RouteMessage 遍历路由表匹配并分发
func RouteMessage(content string, event onebot.Event, groupID string) {
	text := stripCQPrefix(content)
	for _, route := range Routes {
		if strings.HasPrefix(text, route.Keyword) {
			extra := strings.TrimSpace(text[len(route.Keyword):])
			finalPrompt := route.Prompt + route.SharedRules
			if extra != "" {
				finalPrompt += "\n用户补充,优先级很高:" + extra
			}
			enrichedPrompt := fmt.Sprintf("你的QQ号是 %s，昵称是 %s。当前@你的是群友 %s。\n%s", config.BotQQ, config.BotNickname, event.Nickname, finalPrompt)
			route.Handler(event, groupID, enrichedPrompt)
			return
		}
	}
}

// stripCQPrefix 去除消息开头的 CQ 码和 @昵称
func stripCQPrefix(rawMsg string) string {
	text := strings.TrimSpace(rawMsg)
	// 去除 CQ at 码 [CQ:at,qq=xxx]
	if strings.HasPrefix(text, "[CQ:at,qq=") {
		if idx := strings.Index(text, "]"); idx != -1 {
			text = strings.TrimSpace(text[idx+1:])
		}
	}
	// 去除 @机器人昵称
	if config.BotNickname != "" {
		text = strings.TrimPrefix(text, "@"+config.BotNickname)
		text = strings.TrimSpace(text)
	}
	return text
}
