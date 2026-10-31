package workspace

import (
	"context"
	"time"

	"good-review-master/cache"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/pipeline"
	"good-review-master/telemetry"
)

// PrepareBatch 是 Stage 1：按批补全同一会话的消息。
//
// 同一批的发送者与 @ 对象合并成一次批量查库；每条消息仍然独立过滤、独立组装。
// 消息正文在这里同步入库：返回之前必须已经写进 state.db（写失败只降级记日志）。
func (w *Workspace) PrepareBatch(ctx context.Context, events []pipeline.InboundEvent) []pipeline.SyncedMessage {
	if len(events) == 0 {
		return nil
	}
	cfg := w.Snapshot()()
	out := make([]pipeline.SyncedMessage, 0, len(events))

	// 同一批按会话拆开做批量补名；Stage 1 的 key 本来就是一个会话，
	// 这里仍按 chatID 分组，防止调用方把多个会话混进来。
	type batchEntry struct {
		event   onebot.Event
		private bool
		chatID  string
		nameIDs []string
	}
	entries := make([]batchEntry, 0, len(events))
	nameSets := make(map[string][]string)
	seen := make(map[string]struct{}, len(events))

	for _, in := range events {
		event := in.Event
		private := event.IsPrivate()
		chatID := event.ChatID()
		if !private && !w.AllowsGroup(chatID) {
			continue
		}
		gc := w.Cache.GetGroupCache(chatID, cfg.MaxCacheMsg)
		if gc.HasSeen(event.Self.UserID, event.MessageID) {
			logutil.Debug("跳过重复消息", "chat", chatID, "self", event.Self.UserID, "msgID", event.MessageID)
			continue
		}
		key := chatID + "\x00" + event.MessageID
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		if event.Time > 0 {
			telemetry.PollLagSeconds.Observe(time.Since(time.Unix(int64(event.Time), 0)).Seconds())
		}

		nameIDs := make([]string, 0, len(event.Message)+1)
		nameIDs = append(nameIDs, event.UserID)
		if !private {
			w.EnsureGroupFrom(chatID, event.MessageID)
			w.ObserveMemberFrom(chatID, event.UserID, event.MessageID)
		}
		for _, seg := range event.Message {
			if seg.Type != onebot.SegmentMention {
				continue
			}
			userID := seg.String("user_id")
			if userID == "" {
				continue
			}
			nameIDs = append(nameIDs, userID)
			if !private {
				w.ObserveMemberFrom(chatID, userID, event.MessageID)
			}
		}
		if name := event.SenderName(); name != "" {
			if w.LearnMemberNameFrom(chatID, event.UserID, name, event.MessageID) {
				gc.SetNickname(event.UserID, name)
			}
		}
		entries = append(entries, batchEntry{event: event, private: private, chatID: chatID, nameIDs: nameIDs})
		nameSets[chatID] = append(nameSets[chatID], nameIDs...)
	}
	if len(entries) == 0 {
		return nil
	}

	// 每会话一次批量补名（缓存优先，miss 回源 members 表）。
	resolved := make(map[string]map[string]string, len(nameSets))
	for chatID, ids := range nameSets {
		resolved[chatID] = w.ResolveNames(chatID, ids)
	}

	for _, entry := range entries {
		event := entry.event
		nameOf := func(userID string) string {
			if name := resolved[entry.chatID][userID]; name != "" {
				return name
			}
			return w.DisplayNameFor(entry.chatID)(userID)
		}
		content := truncateRunes(event.RenderText(nameOf), cfg.MaxMsgRune)
		if content == "" {
			continue
		}
		replyTo := event.QuotedMessageID()
		if replyTo == "" {
			replyTo = w.State.ResolveMessageID(entry.chatID, event.QuotedRefIdx())
		}
		msg := cache.Message{
			MessageID: event.MessageID,
			GroupID:   entry.chatID,
			SelfID:    event.Self.UserID,
			UserID:    event.UserID,
			Nickname:  nameOf(event.UserID),
			Content:   content,
			Images:    event.Images(),
			ReplyToID: replyTo,
			RefIdx:    event.QQMsgIdx,
			Time:      eventTime(event),
		}
		// Stage 1 同步入库；图片标识等 Stage 2 拿到后再补写。
		// 行号（state.db 自增主键）跟着消息一路带下去，发给大模型的 message_id 就用它。
		msg.RowID = w.State.SaveMessage(msg)
		out = append(out, pipeline.SyncedMessage{
			WorkspaceID: w.ID,
			ChatID:      entry.chatID,
			Private:     entry.private,
			Event:       event,
			Message:     msg,
		})
	}
	return out
}

func eventTime(event onebot.Event) int64 {
	if event.Time > 0 {
		return int64(event.Time)
	}
	return time.Now().Unix()
}

func truncateRunes(text string, limit int) string {
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
