package bot

import (
	"strings"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/onebot"
	"good-review-master/router"
)

// Bot 机器人运行时，管理消息处理与轮询
type Bot struct {
	cfg           *config.Config
	ob            *onebot.Client
	messageRouter *router.Router
}

// NewBot 创建机器人实例
// 字段名用 messageRouter 而不是 router：包本身就叫 router，
// 写成 router *router.Router 虽然合法，但读的人要停顿一下才分得清哪个是包。
func NewBot(cfg *config.Config, ob *onebot.Client, messageRouter *router.Router) *Bot {
	return &Bot{cfg: cfg, ob: ob, messageRouter: messageRouter}
}

// ProcessMessage 处理单条群消息
func (b *Bot) ProcessMessage(event onebot.Event) {
	groupID := event.GroupID
	if !b.cfg.HasGroup(groupID) {
		return
	}

	content, images := onebot.NormalizeContent(event.RawMessage, b.cfg.MaxMsgRune)
	if content == "" {
		return
	}

	msg := cache.Message{
		MsgID:   event.MessageID,
		GroupID: groupID,
		UserID:  event.UserID,
		Nick:    event.Nickname,
		Card:    event.Card,
		Content: content,
		Images:  images,
		Time:    time.Now().Unix(),
	}
	cache.GetGroupCache(groupID, b.cfg.MaxCacheMsg).Add(msg)

	if b.isAtBot(content) {
		b.messageRouter.RouteMessage(content, event, groupID)
	}
}

// isAtBot 检查是否@机器人（QQ号 + 昵称双重校验）
func (b *Bot) isAtBot(rawMsg string) bool {
	if strings.Contains(rawMsg, "[CQ:at,qq="+b.cfg.BotQQ) {
		return true
	}
	// 检查开始是昵称 @
	if b.cfg.BotNickname != "" && strings.HasPrefix(rawMsg, "@"+b.cfg.BotNickname) {
		return true
	}
	return false
}
