package bot

import (
	"context"
	"strconv"
	"strings"
	"time"

	"good-review-master/cache"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

// RunPollingLoop HTTP轮询主循环
func (b *Bot) RunPollingLoop(ctx context.Context) {
	// 首次启动：拉取历史消息填充缓存
	logutil.Info("正在连接 NapCat HTTP API：" + b.cfg.NapCatHTTPAPI)
	for _, groupID := range b.cfg.AllowGroups {
		msgs, err := b.ob.FetchGroupMsgHistory(groupID, b.cfg.MaxCacheMsg)
		if err != nil {
			logutil.Error("首次拉取群消息失败", "group", groupID, "err", err)
			continue
		}
		gc := cache.GetGroupCache(groupID, b.cfg.MaxCacheMsg)
		for _, msg := range msgs {
			content := strings.TrimSpace(msg.RawMessage)
			if content == "" {
				content = ""
			} else if len([]rune(content)) > b.cfg.MaxMsgRune {
				content = string([]rune(content)[:b.cfg.MaxMsgRune]) + "..."
			}
			gc.Add(cache.Message{
				MsgID:   msg.MessageID,
				GroupID: onebot.FormatGroupID(msg.GroupID),
				UserID:  strconv.FormatInt(msg.UserID, 10),
				Nick:    msg.Sender.Nickname,
				Content: content,
				Time:    msg.Time,
			})
		}
		logutil.Info("群消息缓存初始化完成", "group", groupID, "条数", len(msgs))
	}

	logutil.Info("✅ 机器人已上线！轮询中（间隔 " + b.cfg.PollInterval.String() + "）")
	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			logutil.Info("轮询循环正常退出")
			return
		case <-ticker.C:
			for _, groupID := range b.cfg.AllowGroups {
				msgs, err := b.ob.FetchGroupMsgHistory(groupID, 10)
				if err != nil {
					logutil.Error("轮询群消息失败", "group", groupID, "err", err)
					continue
				}

				gc := cache.GetGroupCache(groupID, b.cfg.MaxCacheMsg)
				newCount := 0
				for _, msg := range msgs {
					if gc.HasMsgID(msg.MessageID) {
						continue
					}
					newCount++
					logutil.Info("收到新消息", "group", groupID, "msgID", msg.MessageID, "user", msg.Sender.Nickname, "content", msg.RawMessage)
					b.ProcessMessage(onebot.Event{
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
	}
}
