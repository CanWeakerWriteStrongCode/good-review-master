package bot

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/onebot"
)

// RunPollingLoop HTTP轮询主循环
func RunPollingLoop() {
	// 首次启动：拉取历史消息填充缓存
	slog.Info("正在连接 NapCat HTTP API：" + config.NapCatHTTPAPI)
	for _, g := range strings.Split(config.AllowGroups, ",") {
		groupID := strings.TrimSpace(g)
		if groupID == "" {
			continue
		}
		msgs, err := onebot.FetchGroupMsgHistory(groupID, config.MaxCacheMsg)
		if err != nil {
			slog.Error("首次拉取群消息失败", "group", groupID, "err", err)
			continue
		}
		c := cache.GetGroupCache(groupID)
		for _, m := range msgs {
			content := strings.TrimSpace(m.RawMessage)
			if content == "" || IsPureEmoji(content) {
				content = "" // 过滤的消息记MsgID用于去重，但不存内容
			} else if len([]rune(content)) > config.MaxMsgRune {
				content = string([]rune(content)[:config.MaxMsgRune]) + "..."
			}
			c.Add(cache.Message{
				MsgID:   m.MessageID,
				GroupID: onebot.FormatGroupID(m.GroupID),
				UserID:  strconv.FormatInt(m.UserID, 10),
				Nick:    m.Sender.Nickname,
				Content: content,
				Time:    m.Time,
			})
		}
		slog.Info("群消息缓存初始化完成", "group", groupID, "条数", len(msgs))
	}

	slog.Info("✅ 机器人已上线！轮询中（间隔 " + config.PollInterval.String() + "）")
	ticker := time.NewTicker(config.PollInterval)
	defer ticker.Stop()

	for range ticker.C {
		for _, g := range strings.Split(config.AllowGroups, ",") {
			groupID := strings.TrimSpace(g)
			if groupID == "" {
				continue
			}

			msgs, err := onebot.FetchGroupMsgHistory(groupID, 10)
			if err != nil {
				slog.Error("轮询群消息失败", "group", groupID, "err", err)
				continue
			}

			c := cache.GetGroupCache(groupID)
			newCount := 0
			for _, m := range msgs {
				if c.HasMsgID(m.MessageID) {
					continue
				}
				newCount++
				slog.Info("收到新消息", "group", groupID, "msgID", m.MessageID, "user", m.Sender.Nickname, "content", m.RawMessage)
				ProcessMessage(onebot.Event{
					PostType:    "message",
					MessageType: "group",
					GroupID:     onebot.FormatGroupID(m.GroupID),
					UserID:      strconv.FormatInt(m.UserID, 10),
					Nickname:    m.Sender.Nickname,
					RawMessage:  m.RawMessage,
					MessageID:   m.MessageID,
				})
			}
			if newCount > 0 {
				slog.Debug("本轮轮询结果", "group", groupID, "新消息数", newCount)
			}
		}
	}
}
