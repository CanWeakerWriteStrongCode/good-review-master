package bot

import (
	"context"
	"strings"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/onebot"
	"good-review-master/workspace"
)

// job 一条入站事件与它被解析出的工作空间。
// 两者必须成对入队：消费侧再解析一次会读到不同的账户映射（配置热更新），
// 一条消息的缓存、路由、回复就可能落到两个账号上。
type job struct {
	event onebot.Event
	ws    *workspace.Workspace
}

// Bot 机器人运行时，管理消息接收与处理
type Bot struct {
	cfg        config.Snapshot
	ob         *onebot.Client
	workspaces *workspace.Manager

	// queue 承接 AstrBot 推来的事件。Run 是唯一的消费者，
	// 顺序因此与平台推送顺序一致，不会因为并发处理把群聊上下文打乱。
	queue chan job
}

// NewBot 创建机器人实例。
// 每个工作空间有自己的 Router 与缓存，Bot 只负责解析归属并把事件交给对应的那个。
func NewBot(cfg config.Snapshot, ob *onebot.Client, workspaces *workspace.Manager) *Bot {
	return &Bot{
		cfg:        cfg,
		ob:         ob,
		workspaces: workspaces,
		queue:      make(chan job, eventQueueSize),
	}
}

// ProcessMessage 处理单条群消息
//
// 开头取一次快照、全程用同一个局部变量：这样一条消息的整个处理过程
// （白名单判定、截断长度、缓冲上限、@检测）看到的是**同一份**配置。
// 若每处都调 b.cfg()，热更新可能正好插在中间，导致一半按旧配置一半按新配置执行。
func (b *Bot) ProcessMessage(ctx context.Context, event onebot.Event, ws *workspace.Workspace) {
	if ws == nil {
		return
	}
	cfg := ws.Snapshot()()
	groupID := event.GroupID
	if !ws.AllowsGroup(groupID) {
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
		MessageID: event.MessageID,
		GroupID:   groupID,
		SelfID:    event.SelfID,
		UserID:    event.UserID,
		Nickname:  event.DisplayName(),
		Card:      event.Sender.Card,
		Content:   content,
		Images:    images,
		ReplyToID: event.QuotedMessageID(),
		Time:      eventTime(event),
	}
	ws.Cache.GetGroupCache(groupID, cfg.MaxCacheMsg).Add(msg)

	if event.Mentioned || isAtBot(event, content, cfg, b.allowNicknameFallback(ws, cfg.BotNickname)) {
		// 把消费循环的 ctx 传下去：一次"消息 → 路由 → 大模型 → 工具调用"
		// 因此成为同一条链路（trace span 能串起来），关机也能取消在途调用。
		ws.Router.RouteMessage(ctx, content, event, groupID)
	}
}

// allowNicknameFallback 昵称兜底只在昵称唯一归属一个工作空间时可信。
// 两个账号用同一个昵称时，"@昵称"无法区分是谁，只能用稳定 ID 判断。
func (b *Bot) allowNicknameFallback(ws *workspace.Workspace, nickname string) bool {
	if nickname == "" {
		return false
	}
	return len(b.workspaces.List()) == 1 || b.workspaces.NicknameUnique(nickname)
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

// isAtBot 检查是否@机器人（稳定 ID 优先 + 唯一昵称兜底）。
// AstrBot 桥接层已经算好 mentioned，这里只服务于：
//   - 桥接层漏判，但原始消息里确实带了机器人的 openid/QQ；
//   - raw_message 里是手打的 "@昵称" 文本。
//
// cfg 是**当前工作空间**的快照；allowNickname 由调用方按"昵称是否唯一归属"给出。
// 多账号同名时禁用昵称兜底，否则 @ 另一个账号会让本账号抢答。
func isAtBot(event onebot.Event, content string, cfg *config.Config, allowNickname bool) bool {
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
	if allowNickname && cfg.BotNickname != "" && strings.HasPrefix(content, "@"+cfg.BotNickname) {
		return true
	}
	return strings.HasPrefix(content, "@你")
}
