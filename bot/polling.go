package bot

import (
	"context"
	"strconv"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

// RunPollingLoop HTTP轮询主循环
func (b *Bot) RunPollingLoop(ctx context.Context) {
	// 首次启动：拉取历史消息填充缓存
	initial := b.cfg()
	logutil.Info("正在连接 NapCat HTTP API：" + initial.NapCatHTTPAPI)
	const historyCount = 30
	backfillHistory(initial, b.ob, historyCount)

	interval := initial.PollInterval
	logutil.Info("✅ 机器人已上线！轮询中（间隔 " + interval.String() + "）")
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logutil.Info("轮询循环正常退出")
			return
		case <-ticker.C:
			cfg := b.cfg()
			// 轮询间隔也是可热更新的：ticker 建好之后不会自己跟着配置变，
			// 只有 Reset 才换。放在这里是刻意的——它必须在"下一轮"才生效，
			// 不能中途打断已经排队的这一次 tick。
			if cfg.PollInterval != interval {
				interval = cfg.PollInterval
				ticker.Reset(interval)
				logutil.Info("轮询间隔已更新", "新间隔", interval.String())
			}
			pollOnce(cfg, b.ob, historyCount, b.ProcessMessage)
		}
	}
}

// backfillHistory 首次启动时拉取历史消息填充缓存。
func backfillHistory(cfg *config.Config, ob *onebot.Client, count int) {
	for _, groupID := range cfg.AllowGroups {
		msgs, err := ob.FetchGroupMsgHistory(groupID, count)
		if err != nil {
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
	}
}

// pollOnce 拉取一轮全部白名单群的新消息。
//
// cfg 与 handle 都由调用方传入，而不是从 b 上取：这样"这一轮用的配置"
// 与"处理消息时用的配置"是同一份，不会一轮之内换掉。
func pollOnce(cfg *config.Config, ob *onebot.Client, count int, handle func(onebot.Event)) {
	for _, groupID := range cfg.AllowGroups {
		msgs, err := ob.FetchGroupMsgHistory(groupID, count)
		if err != nil {
			logutil.Error("轮询群消息失败", "group", groupID, "err", err)
			continue
		}

		gc := cache.GetGroupCache(groupID, cfg.MaxCacheMsg)
		newCount := 0
		for _, msg := range msgs {
			if gc.HasMsgID(msg.MessageID) {
				continue
			}
			newCount++
			logutil.Info("收到新消息", "group", groupID, "msgID", msg.MessageID, "user", msg.Sender.Nickname, "content", msg.RawMessage)
			handle(onebot.Event{
				PostType:    "message",
				MessageType: "group",
				GroupID:     onebot.FormatGroupID(msg.GroupID),
				UserID:      strconv.FormatInt(msg.UserID, 10),
				Nickname:    msg.Sender.Nickname,
				RawMessage:  msg.RawMessage,
				MessageID:   msg.MessageID,
			})
		}
		if newCount > 0 {
			logutil.Debug("本轮轮询结果", "group", groupID, "新消息数", newCount)
		}
	}
}
