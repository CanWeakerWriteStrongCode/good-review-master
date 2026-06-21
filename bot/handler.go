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
	if IsPureEmoji(content) {
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

// IsPureEmoji 检查是否纯表情/无实质文本内容
func IsPureEmoji(text string) bool {
	for _, r := range text {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			(r >= 0x4e00 && r <= 0x9fff) {
			return false
		}
	}
	return true
}

// isAtBot 检查是否@机器人
func isAtBot(rawMsg string) bool {
	return strings.Contains(rawMsg, config.BotQQ)
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
