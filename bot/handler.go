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
	cfg           config.Snapshot
	ob            *onebot.Client
	messageRouter *router.Router
}

// NewBot 创建机器人实例
// 字段名用 messageRouter 而不是 router：包本身就叫 router，
// 写成 router *router.Router 虽然合法，但读的人要停顿一下才分得清哪个是包。
func NewBot(cfg config.Snapshot, ob *onebot.Client, messageRouter *router.Router) *Bot {
	return &Bot{cfg: cfg, ob: ob, messageRouter: messageRouter}
}

// ProcessMessage 处理单条群消息
//
// 开头取一次快照、全程用同一个局部变量：这样一条消息的整个处理过程
// （白名单判定、截断长度、缓冲上限、@检测）看到的是**同一份**配置。
// 若每处都调 b.cfg()，热更新可能正好插在中间，导致一半按旧配置一半按新配置执行。
func (b *Bot) ProcessMessage(event onebot.Event) {
	cfg := b.cfg()
	groupID := event.GroupID
	if !cfg.HasGroup(groupID) {
		return
	}

	content, images := onebot.NormalizeContent(event.RawMessage, cfg.MaxMsgRune)
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
	cache.GetGroupCache(groupID, cfg.MaxCacheMsg).Add(msg)

	if isAtBot(content, cfg) {
		b.messageRouter.RouteMessage(content, event, groupID)
	}
}

// isAtBot 检查是否@机器人（QQ号 + 昵称双重校验）。
// cfg 由调用方传进来（就是 ProcessMessage 开头取的那一份），不在这里再取快照，
// 免得"截断用的配置"和"@检测用的配置"来自两次不同的读取。
func isAtBot(rawMsg string, cfg *config.Config) bool {
	if strings.Contains(rawMsg, "[CQ:at,qq="+cfg.BotQQ) {
		return true
	}
	// 检查开始是昵称 @
	if cfg.BotNickname != "" && strings.HasPrefix(rawMsg, "@"+cfg.BotNickname) {
		return true
	}
	return false
}
