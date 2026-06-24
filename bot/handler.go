package bot

import (
	"strings"
	"time"

	"good-review-master/cache"
	"good-review-master/cmd"
	"good-review-master/config"
	"good-review-master/onebot"
)

// ProcessMessage 处理单条群消息
func ProcessMessage(event onebot.Event) {
	groupID := event.GroupID
	if !isAllowGroup(groupID) {
		return
	}

	content := strings.TrimSpace(event.RawMessage)
	if content == "" {
		return
	}
	if len([]rune(content)) > config.MaxMsgRune {
		content = string([]rune(content)[:config.MaxMsgRune]) + "..."
	}

	msg := cache.Message{
		MsgID:   event.MessageID,
		GroupID: groupID,
		UserID:  event.UserID,
		Nick:    event.Nickname,
		Content: content,
		Time:    time.Now().Unix(),
	}
	cache.GetGroupCache(groupID).Add(msg)

	if isAtBot(content) {
		cmd.RouteMessage(content, event, groupID)
	}
}

// isAtBot 检查是否@机器人（QQ号 + 昵称双重校验）
func isAtBot(rawMsg string) bool {
	if strings.Contains(rawMsg, config.BotQQ) {
		return true
	}
	if config.BotNickname != "" && strings.Contains(rawMsg, "@"+config.BotNickname) {
		return true
	}
	return false
}

// isAllowGroup 检查群白名单
func isAllowGroup(groupID string) bool {
	for _, g := range strings.Split(config.AllowGroups, ",") {
		if strings.TrimSpace(g) == groupID {
			return true
		}
	}
	return false
}
