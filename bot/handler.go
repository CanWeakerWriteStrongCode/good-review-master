package bot

import (
	"context"
	"strings"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/onebot"
	"good-review-master/router"
)

// Bot 机器人运行时，管理消息接收与处理
type Bot struct {
	cfg           config.Snapshot
	ob            *onebot.Client
	messageRouter *router.Router

	// queue 承接 AstrBot 推来的事件。Run 是唯一的消费者，
	// 顺序因此与平台推送顺序一致，不会因为并发处理把群聊上下文打乱。
	queue chan onebot.Event
}

// NewBot 创建机器人实例
// 字段名用 messageRouter 而不是 router：包本身就叫 router，
// 写成 router *router.Router 虽然合法，但读的人要停顿一下才分得清哪个是包。
func NewBot(cfg config.Snapshot, ob *onebot.Client, messageRouter *router.Router) *Bot {
	return &Bot{
		cfg:           cfg,
		ob:            ob,
		messageRouter: messageRouter,
		queue:         make(chan onebot.Event, eventQueueSize),
	}
}

// ProcessMessage 处理单条群消息
//
// 开头取一次快照、全程用同一个局部变量：这样一条消息的整个处理过程
// （白名单判定、截断长度、缓冲上限、@检测）看到的是**同一份**配置。
// 若每处都调 b.cfg()，热更新可能正好插在中间，导致一半按旧配置一半按新配置执行。
func (b *Bot) ProcessMessage(ctx context.Context, event onebot.Event) {
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
		Time:    eventTime(event),
	}
	cache.GetGroupCache(groupID, cfg.MaxCacheMsg).Add(msg)

	if event.Mentioned || isAtBot(content, cfg) {
		// 把消费循环的 ctx 传下去：一次"消息 → 路由 → 大模型 → 工具调用"
		// 因此成为同一条链路（trace span 能串起来），关机也能取消在途调用。
		b.messageRouter.RouteMessage(ctx, content, event, groupID)
	}
}

// eventTime 优先使用平台事件时间；缺失时退回本地时钟。
// 用平台时间而不是 time.Now()：缓存排序与延迟指标要反映消息真正发出的时刻，
// 否则一次重连补推会让一堆旧消息看起来"刚刚发生"。
func eventTime(event onebot.Event) int64 {
	if event.Time > 0 {
		return event.Time
	}
	return time.Now().Unix()
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
