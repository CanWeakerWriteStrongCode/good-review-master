package bot

import (
	"strings"
	"time"

	"good-review-master/cache"
	"good-review-master/cmd"
	"good-review-master/config"
	"good-review-master/onebot"
)

// Bot 机器人运行时，管理消息处理与轮询
type Bot struct {
	cfg    *config.Config
	ob     *onebot.Client
	router *cmd.Router
}

// NewBot 创建机器人实例
func NewBot(cfg *config.Config, ob *onebot.Client, router *cmd.Router) *Bot {
	return &Bot{cfg: cfg, ob: ob, router: router}
}

// ProcessMessage 处理单条群消息
func (b *Bot) ProcessMessage(event onebot.Event) {
	groupID := event.GroupID
	if !b.cfg.HasGroup(groupID) {
		return
	}

	content := strings.TrimSpace(event.RawMessage)
	if content == "" {
		return
	}
	if len([]rune(content)) > b.cfg.MaxMsgRune {
		content = string([]rune(content)[:b.cfg.MaxMsgRune]) + "..."
	}

	msg := cache.Message{
		MsgID:   event.MessageID,
		GroupID: groupID,
		UserID:  event.UserID,
		Nick:    event.Nickname,
		Content: content,
		Time:    time.Now().Unix(),
	}
	cache.GetGroupCache(groupID, b.cfg.MaxCacheMsg).Add(msg)

	if b.isAtBot(content) {
		b.router.RouteMessage(content, event, groupID)
	}
}

// isAtBot 检查是否@机器人（QQ号 + 昵称双重校验）
func (b *Bot) isAtBot(rawMsg string) bool {
	if strings.Contains(rawMsg, b.cfg.BotQQ) {
		return true
	}
	if b.cfg.BotNickname != "" && strings.Contains(rawMsg, "@"+b.cfg.BotNickname) {
		return true
	}
	return false
}
