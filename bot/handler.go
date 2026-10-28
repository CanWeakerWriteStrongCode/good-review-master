package bot

import (
	"context"
	"strings"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/onebot"
	"good-review-master/telemetry"
	"good-review-master/workspace"
)

// job 一条入站事件与它被解析出的工作空间。
// 两者必须成对入队：消费侧再解析一次会读到不同的账号映射（配置热更新），
// 一条消息的缓存、路由、回复就可能落到两个账号上。
type job struct {
	event onebot.Event
	ws    *workspace.Workspace
}

// Bot 机器人运行时，管理消息接收与处理
type Bot struct {
	cfg        config.Snapshot
	workspaces *workspace.Manager

	// queue 承接实现端推来的事件。Run 是唯一的消费者，
	// 顺序因此与平台推送顺序一致，不会因为并发处理把群聊上下文打乱。
	queue chan job
}

// NewBot 创建机器人实例。
// 每个工作空间有自己的 Router 与缓存，Bot 只负责解析归属并把事件交给对应的那个。
func NewBot(cfg config.Snapshot, workspaces *workspace.Manager) *Bot {
	return &Bot{
		cfg:        cfg,
		workspaces: workspaces,
		queue:      make(chan job, eventQueueSize),
	}
}

// ProcessMessage 处理单条群消息
//
// 开头取一次快照、全程用同一个局部变量：这样一条消息的整个处理过程
// （白名单判定、截断长度、缓冲上限、@检测）看到的是**同一份**配置。
func (b *Bot) ProcessMessage(ctx context.Context, event onebot.Event, ws *workspace.Workspace) {
	if ws == nil {
		return
	}
	cfg := ws.Snapshot()()
	groupID := event.GroupID
	if !ws.AllowsGroup(groupID) {
		return
	}

	// 群档案与成员目录跟着消息一起积累：群只写一次，成员首次出现时写一行。
	ws.EnsureGroup(groupID)
	ws.ObserveMember(groupID, event.UserID)
	for _, seg := range event.Message {
		if seg.Type == onebot.SegmentMention {
			ws.ObserveMember(groupID, seg.String("user_id"))
		}
	}

	nameOf := ws.DisplayNameFor(groupID)
	content := truncate(event.RenderText(nameOf), cfg.MaxMsgRune)
	if content == "" {
		return
	}

	// QQ 官方群消息的引用只给被引消息的 REFIDX（qq.ref_msg_idx），不给 message_id；
	// 用已经存下的 ref_idx 反查一次，把 reply_to 补成真正的 message_id。
	replyTo := event.QuotedMessageID()
	if replyTo == "" {
		replyTo = ws.State.ResolveMessageID(groupID, event.QuotedRefIdx())
	}

	msg := cache.Message{
		MessageID: event.MessageID,
		GroupID:   groupID,
		SelfID:    event.Self.UserID,
		UserID:    event.UserID,
		Nickname:  nameOf(event.UserID),
		Content:   content,
		Images:    event.Images(),
		ReplyToID: replyTo,
		RefIdx:    event.QQMsgIdx,
		Time:      eventTime(event),
	}
	gc := ws.Cache.GetGroupCache(groupID, cfg.MaxCacheMsg)
	gc.Add(msg)
	telemetry.CacheMessages.WithLabelValues(ws.ID, groupID).Set(float64(gc.Len()))
	ws.State.SaveMessage(msg)
	// self.user_id 按群学习（QQ 官方是群作用域 openid），重启后仍能对 @ 做比较。
	// 只有值变化时才写库，避免每条消息一次同步 INSERT。
	ws.LearnIdentity(groupID, event.Self.UserID)
	// 借本条消息的身份核对一次机器人昵称（平台侧改名后 state.db 的缓存不会自己失效）。
	// 传 self_id + group_id：多账号共用一个实现端时，只有它们能让实现端定位到本机器人。
	ws.SyncSelfName(event.BotID(), groupID)

	// 触发优先级：标准 mention > 实现端显式 @ 标记（无 openid 时）> 昵称兜底。
	// 收到事件不等于被 @，@全体也不等于 @ 本账号。
	if event.MentionsBot() || event.AdapterAtMe() ||
		isAtBot(content, cfg, b.allowNicknameFallback(ws, cfg.BotNickname)) {
		// 把消费循环的 ctx 传下去：一次"消息 → 路由 → 大模型 → 工具调用"
		// 因此成为同一条链路（trace span 能串起来），关机也能取消在途调用。
		ws.Router.RouteMessage(ctx, content, event, groupID)
		return
	}

	// 没被点名：交给主动插话链路，由大模型裁决要不要接话。
	// 门控、冷却、串行 worker 全在 Router 内，功能关闭时这里是空操作。
	ws.Router.TryProactive(ctx, event, groupID, content)
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
		return int64(event.Time)
	}
	return time.Now().Unix()
}

// isAtBot 检查是否 @ 机器人。标准 mention 由调用方先用 event.MentionsBot() 判定，
// 这里只处理两类兜底：
//   - 适配器没有产出 mention 段，只把 @ 写进了文本（渲染成「@昵称」）；
//   - 用户手打的 "@昵称" 文本。
//
// allowNickname 由调用方按"昵称是否唯一归属"给出：同名账号下 @ 无法区分是谁。
func isAtBot(content string, cfg *config.Config, allowNickname bool) bool {
	if allowNickname && cfg.BotNickname != "" && strings.HasPrefix(content, "@"+cfg.BotNickname) {
		return true
	}
	return strings.HasPrefix(content, "@你")
}

// truncate 按字符数截断（不是字节），避免把多字节字符切成乱码。
func truncate(text string, limit int) string {
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
