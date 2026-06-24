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
	for _, r := range Routes {
		if strings.HasPrefix(text, r.Keyword) {
			extra := strings.TrimSpace(text[len(r.Keyword):])
			finalPrompt := r.Prompt + r.SharedRules
			if extra != "" {
				finalPrompt += "\n用户补充,优先级很高:" + extra
			}
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
