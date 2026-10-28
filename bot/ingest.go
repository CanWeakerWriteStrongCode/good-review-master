package bot

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/telemetry"
	"good-review-master/workspace"
)

const (
	// eventQueueSize 是入站事件队列容量。群聊消息量很低，这个值足够吸收
	// 启动回填期间和一次重连风暴；真正的背压由 AcceptEvent 的等待超时给出。
	eventQueueSize = 1024

	// enqueueTimeout HTTP 处理器最多等这么久。超过就回 503，
	// 让桥接层把"下游处理不过来"显式暴露出来，而不是悄悄丢掉消息。
	enqueueTimeout = 2 * time.Second

	// historyCount 启动时回填的历史消息条数。
	historyCount = 30
)

// Run 启动消息消费循环：先回填历史，再按到达顺序处理 AstrBot 推送的事件。
//
// 顺序不能反。回填是"补过去"，推送是"接现在"；先回填再消费队列，
// 缓存里的时间顺序才是连续的。反过来会出现新消息先入库、旧消息后插入，
// 大模型看到的聊天记录顺序就乱了。
func (b *Bot) Run(ctx context.Context) {
	initial := b.cfg()
	if initial.BackfillOnStart {
		if len(b.workspaces.List()) > 1 {
			logutil.Warn("多工作空间下跳过启动回填：历史接口不区分接收账号，回填会把同一个群的数据写进错误的工作空间")
		} else {
			logutil.Info("正在通过 AstrBot 桥接拉取历史消息", "api", initial.AstrBotHTTPAPI)
			backfillHistory(initial, b.workspaces.List()[0], b.ob, historyCount)
		}
	} else {
		logutil.Info("已关闭启动历史回填（当前平台没有群历史消息接口）")
	}
	logutil.Info("🤖 机器人已就绪，等待 IM 事件推送")

	for {
		select {
		case <-ctx.Done():
			logutil.Info("消息分发循环正常退出")
			return
		case item := <-b.queue:
			b.dispatch(ctx, item)
		}
	}
}

// AcceptEvent 把一条事件放进队列后立即返回。
//
// 不在 HTTP 处理器里直接处理：一次 @ 会触发大模型调用，让桥接层等上几十秒没有任何好处，
// 而且会让 AstrBot 的事件循环跟着阻塞。
//
// 账号白名单在这里判定而不是消费侧：桥接插件要根据 accepted 决定是否 stop_event()，
// 不在 Go 管辖范围内的账号必须让它继续走到 AstrBot 自己的 Agent。
func (b *Bot) AcceptEvent(ctx context.Context, event onebot.Event) (bool, error) {
	// allow_self_ids 为空且没有工作空间 = 一个账号都不接管（旧语义，必须保留）。
	// 配了工作空间后 HasSelfID 返回 true，接管范围交给下面的归属解析。
	if !b.cfg().HasSelfID(event.SelfID) {
		return false, nil
	}
	// 归属解析必须发生在入队前：解析不到就不接管（交还 AstrBot Agent），
	// 而不是先收下再在消费侧猜一个账号。
	ws := b.workspaces.Resolve(event)
	if ws == nil {
		logutil.Warn("无法确定事件所属工作空间，已拒绝接管",
			"platform_id", event.PlatformID, "self_id", event.SelfID,
			"group", event.GroupID, "user", event.UserID)
		return false, nil
	}
	event = ws.BindEvent(event)

	timer := time.NewTimer(enqueueTimeout)
	defer timer.Stop()

	select {
	case b.queue <- job{event: event, ws: ws}:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, fmt.Errorf("消息队列已满（容量 %d）", eventQueueSize)
	}
}

// dispatch 做消费侧的最后一层过滤：白名单与重复消息。
//
// 去重必须放在消费侧而不是入队侧：事件可能在历史回填完成前就入队，
// 入队时缓存还没填到这条消息；等真正处理时再看，才能同时覆盖
// "重连后平台重发"和"历史回填与推送重叠"两种情况。
func (b *Bot) dispatch(ctx context.Context, item job) {
	event, ws := item.event, item.ws
	cfg := ws.Snapshot()()
	if !ws.AllowsGroup(event.GroupID) {
		return
	}
	// 另一个被 Go 接管的机器人账号发的消息：既不进上下文，也不能触发回复。
	if cfg.IsManagedAccount(event.UserID) || b.workspaces.IsManagedAccount(event.UserID) {
		return
	}

	gc := ws.Cache.GetGroupCache(event.GroupID, cfg.MaxCacheMsg)
	if gc.HasSeen(event.SelfID, event.MessageID) {
		logutil.Debug("跳过重复消息", "group", event.GroupID, "self", event.SelfID, "msgID", event.MessageID)
		return
	}

	if event.Time > 0 {
		telemetry.PollLagSeconds.Observe(time.Since(time.Unix(event.Time, 0)).Seconds())
	}
	b.ProcessMessage(ctx, event, ws)
}

// backfillHistory 首次启动时拉取历史消息填充缓存。
func backfillHistory(cfg *config.Config, ws *workspace.Workspace, ob *onebot.Client, count int) {
	for _, groupID := range ws.Snapshot()().AllowGroups {
		if groupID == "*" {
			// 通配符无法枚举具体群：启动时缓存还是空的，没有可回填的对象。
			logutil.Info("allow_groups 含通配符 *，跳过启动回填（无法枚举全部群）")
			continue
		}
		msgs, err := ob.FetchGroupMsgHistory(groupID, count)
		if err != nil {
			telemetry.PollFetchErrorsTotal.WithLabelValues(groupID).Inc()
			logutil.Error("首次拉取群消息失败", "group", groupID, "err", err)
			continue
		}
		gc := ws.Cache.GetGroupCache(groupID, cfg.MaxCacheMsg)
		for _, msg := range msgs {
			content, images := onebot.NormalizeContent(msg.RawMessage, cfg.MaxMsgRune)
			gc.Add(cache.Message{
				MessageID: msg.MessageID,
				GroupID:   onebot.FormatGroupID(msg.GroupID),
				UserID:    strconv.FormatInt(msg.UserID, 10),
				Nickname:  msg.Sender.Nickname,
				Card:      msg.Sender.Card,
				Content:   content,
				Images:    images,
				Time:      msg.Time,
			})
		}
		logutil.Info("群消息缓存初始化完成", "group", groupID, "条数", len(msgs))
		telemetry.CacheMessages.WithLabelValues(ws.ID, groupID).Set(float64(gc.Len()))
	}
}
