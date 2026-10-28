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
		logutil.Info("正在通过 AstrBot 桥接拉取历史消息", "api", initial.NapCatHTTPAPI)
		backfillHistory(initial, b.ob, historyCount)
	} else {
		logutil.Info("已关闭启动历史回填（当前平台没有群历史消息接口）")
	}
	logutil.Info("🤖 机器人已就绪，等待 IM 事件推送")

	for {
		select {
		case <-ctx.Done():
			logutil.Info("消息分发循环正常退出")
			return
		case event := <-b.queue:
			b.dispatch(ctx, event)
		}
	}
}

// AcceptEvent 把一条事件放进队列后立即返回。
//
// 不在 HTTP 处理器里直接处理：一次 @ 会触发大模型调用，让桥接层等上几十秒没有任何好处，
// 而且会让 AstrBot 的事件循环跟着阻塞。
func (b *Bot) AcceptEvent(ctx context.Context, event onebot.Event) error {
	timer := time.NewTimer(enqueueTimeout)
	defer timer.Stop()

	select {
	case b.queue <- event:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return fmt.Errorf("消息队列已满（容量 %d）", eventQueueSize)
	}
}

// dispatch 做消费侧的最后一层过滤：白名单与重复消息。
//
// 去重必须放在消费侧而不是入队侧：事件可能在历史回填完成前就入队，
// 入队时缓存还没填到这条消息；等真正处理时再看，才能同时覆盖
// "重连后平台重发"和"历史回填与推送重叠"两种情况。
func (b *Bot) dispatch(ctx context.Context, event onebot.Event) {
	cfg := b.cfg()
	if !cfg.HasGroup(event.GroupID) {
		return
	}

	gc := cache.GetGroupCache(event.GroupID, cfg.MaxCacheMsg)
	if event.MessageID != 0 && gc.HasMsgID(event.MessageID) {
		logutil.Debug("跳过重复消息", "group", event.GroupID, "msgID", event.MessageID)
		return
	}

	if event.Time > 0 {
		telemetry.PollLagSeconds.Observe(time.Since(time.Unix(event.Time, 0)).Seconds())
	}
	b.ProcessMessage(ctx, event)
}

// backfillHistory 首次启动时拉取历史消息填充缓存。
func backfillHistory(cfg *config.Config, ob *onebot.Client, count int) {
	for _, groupID := range cfg.AllowGroups {
		msgs, err := ob.FetchGroupMsgHistory(groupID, count)
		if err != nil {
			telemetry.PollFetchErrorsTotal.WithLabelValues(groupID).Inc()
			logutil.Error("首次拉取群消息失败", "group", groupID, "err", err)
			continue
		}
		gc := cache.GetGroupCache(groupID, cfg.MaxCacheMsg)
		for _, msg := range msgs {
			content, images := onebot.NormalizeContent(msg.RawMessage, cfg.MaxMsgRune)
			gc.Add(cache.Message{
				MsgID:   msg.MessageID,
				GroupID: onebot.FormatGroupID(msg.GroupID),
				UserID:  strconv.FormatInt(msg.UserID, 10),
				Nick:    msg.Sender.Nickname,
				Card:    msg.Sender.Card,
				Content: content,
				Images:  images,
				Time:    msg.Time,
			})
		}
		logutil.Info("群消息缓存初始化完成", "group", groupID, "条数", len(msgs))
		telemetry.CacheMessages.WithLabelValues(groupID).Set(float64(gc.Len()))
	}
}
