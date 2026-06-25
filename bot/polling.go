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
	for _, group := range strings.Split(config.AllowGroups, ",") {
		groupID := strings.TrimSpace(group)
		if groupID == "" {
			continue
		}
		msgs, err := onebot.FetchGroupMsgHistory(groupID, config.MaxCacheMsg)
		if err != nil {
			slog.Error("首次拉取群消息失败", "group", groupID, "err", err)
			continue
		}
		gc := cache.GetGroupCache(groupID)
		for _, msg := range msgs {
			content := strings.TrimSpace(msg.RawMessage)
			if content == "" {
				content = ""
			} else if len([]rune(content)) > config.MaxMsgRune {
				content = string([]rune(content)[:config.MaxMsgRune]) + "..."
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
		slog.Info("群消息缓存初始化完成", "group", groupID, "条数", len(msgs))
	}

	slog.Info("✅ 机器人已上线！轮询中（间隔 " + config.PollInterval.String() + "）")
	ticker := time.NewTicker(config.PollInterval)
	defer ticker.Stop()

	for range ticker.C {
		for _, group := range strings.Split(config.AllowGroups, ",") {
			groupID := strings.TrimSpace(group)
			if groupID == "" {
				continue
			}

			msgs, err := onebot.FetchGroupMsgHistory(groupID, 10)
			if err != nil {
				slog.Error("轮询群消息失败", "group", groupID, "err", err)
				continue
			}

			gc := cache.GetGroupCache(groupID)
			newCount := 0
			for _, msg := range msgs {
				if gc.HasMsgID(msg.MessageID) {
					continue
				}
				newCount++
				slog.Info("收到新消息", "group", groupID, "msgID", msg.MessageID, "user", msg.Sender.Nickname, "content", msg.RawMessage)
				ProcessMessage(onebot.Event{
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
				slog.Debug("本轮轮询结果", "group", groupID, "新消息数", newCount)
			}
		}
	}
}
