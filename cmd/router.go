package cmd

import (
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

// Routes 路由表（扩展新功能只需在这里加一行）
var Routes = []Route{
	{Keyword: config.CmdConfigs["sharptake"].Keyword, Prompt: config.CmdConfigs["sharptake"].Prompt, Handler: sharpTake},
	{Keyword: config.CmdConfigs["whoami"].Keyword, Prompt: config.CmdConfigs["whoami"].Prompt, Handler: whoami},
}

// RouteMessage 遍历路由表匹配并分发
func RouteMessage(content string, event onebot.Event, groupID string) {
	for _, r := range Routes {
		if strings.Contains(content, r.Keyword) {
			r.Handler(event, groupID, r.Prompt)
			return
		}
	}
}
