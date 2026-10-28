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

	rawContent, images := onebot.NormalizeContent(event.RawMessage, cfg.MaxMsgRune)
	content := rawContent
	// DisplayMessage 是桥接插件翻译好的「@昵称(openid)」文本；只有它为空时才退回 raw。
	if event.DisplayMessage != "" {
		if display, _ := onebot.NormalizeContent(event.DisplayMessage, cfg.MaxMsgRune); display != "" {
			content = display
		}
	}
	content = onebot.NormalizeMentions(content, event.BotID(), cfg.BotNickname)
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
		QuoteID: event.QuoteMessageID(),
		Time:    eventTime(event),
	}
	cache.GetGroupCache(groupID, cfg.MaxCacheMsg).Add(msg)

	if event.Mentioned || isAtBot(event, content, cfg) {
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

// isAtBot 检查是否@机器人（稳定 ID + 昵称兜底）。
// AstrBot 桥接层已经算好 mentioned，这里只服务于：
//   - 桥接层漏判，但原始消息里确实带了机器人的 openid/QQ；
//   - raw_message 里是手打的 "@昵称" 文本。
//
// 不再假设 QQ 官方机器人有数字 QQ 号：BotID 可能来自 self_openid，也可能是 aiocqhttp 的数字 ID。
// cfg 由调用方传进来（就是 ProcessMessage 开头取的那一份），不在这里再取快照，
// 免得"截断用的配置"和"@检测用的配置"来自两次不同的读取。
func isAtBot(event onebot.Event, content string, cfg *config.Config) bool {
	if botID := event.BotID(); botID != "" {
		if strings.Contains(event.RawMessage, "[CQ:at,qq="+botID) ||
			strings.Contains(event.RawMessage, "<@"+botID+">") ||
			strings.Contains(event.RawMessage, "<@!"+botID+">") {
			return true
		}
		// 桥接层已翻译成「@昵称(ID)」时，直接把 ID 对上即可
		if strings.Contains(content, "("+botID+")") {
			return true
		}
	}
	if cfg.BotNickname != "" && strings.HasPrefix(content, "@"+cfg.BotNickname) {
		return true
	}
	return strings.HasPrefix(content, "@你")
}
