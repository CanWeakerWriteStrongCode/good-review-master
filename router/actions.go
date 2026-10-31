package router

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

// ReplyAction 一次回复里的一个发送动作。
// ID 由“入站消息 + 第几句”确定，重试时用它做幂等。
type ReplyAction struct {
	ID               string
	ReplyToMessageID string
	RefIdx           string
	Text             string
	ChatID           string
	UserID           string
	Private          bool
	Mention          bool
	Quote            bool // 第一条动作引用触发消息
}

// LedgerEntry 发送台账的一行。Sent=true 含“结果不确定”；
// SendOK=false 表示接口没有明确返回成功，后台需要排查。
type LedgerEntry struct {
	ActionID  string
	MessageID string
	Text      string
	Sent      bool
	SendOK    bool
}

// SendLedger 发送台账。生产实现落 state.db；未注入时用进程内 map。
type SendLedger interface {
	Load(actionID string) (LedgerEntry, bool, error)
	Mark(entry LedgerEntry)
}

// memoryLedger 进程内台账：跨重启会丢，但同一进程内的重试不会重复发送。
type memoryLedger struct {
	mu      sync.Mutex
	entries map[string]LedgerEntry
}

// NewMemoryLedger 创建进程内发送台账。
func NewMemoryLedger() SendLedger {
	return &memoryLedger{entries: make(map[string]LedgerEntry)}
}

func (l *memoryLedger) Load(actionID string) (LedgerEntry, bool, error) {
	if l == nil {
		return LedgerEntry{}, false, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.entries[actionID]
	return entry, ok, nil
}

func (l *memoryLedger) Mark(entry LedgerEntry) {
	if l == nil || entry.ActionID == "" {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.entries[entry.ActionID] = entry
}

// buildReplyActions 把排版后的句子转成动作列表。
// ReplySendInterval<=0 或只有一句时合成一个动作（旧行为）。
func buildReplyActions(workspace string, event onebot.Event, chatID string, mention bool, sentences []string, cfg *config.Config) []ReplyAction {
	if len(sentences) == 0 {
		return nil
	}
	chunks := splitReplyMessages(sentences, maxReplyMessages)
	if cfg.ReplySendInterval <= 0 || len(sentences) == 1 {
		chunks = [][]string{sentences}
	}
	actions := make([]ReplyAction, 0, len(chunks))
	for i, chunk := range chunks {
		actions = append(actions, ReplyAction{
			ID:               fmt.Sprintf("%s|%s|%d", workspace, event.MessageID, i),
			ReplyToMessageID: event.MessageID,
			RefIdx:           event.QQMsgIdx,
			Text:             joinLines(chunk),
			ChatID:           chatID,
			UserID:           event.UserID,
			Private:          event.IsPrivate(),
			Mention:          i == 0 && mention,
			Quote:            i == 0,
		})
	}
	return actions
}

func joinLines(lines []string) string {
	out := ""
	for i, line := range lines {
		if i > 0 {
			out += "\n"
		}
		out += line
	}
	return out
}

// sendAction 发送一个动作，返回台账条目。
// 传输错误（超时/连接中断）按“已发但未确认”处理，不重发；平台明确拒绝才算未发。
func (r *Router) sendAction(ctx context.Context, action ReplyAction) (LedgerEntry, error) {
	entry := LedgerEntry{ActionID: action.ID, MessageID: action.ReplyToMessageID, Text: action.Text}
	var err error
	switch {
	case action.Private:
		err = r.obClient.SendPrivateReplyE(action.UserID, action.Text)
	case action.Mention:
		err = r.obClient.SendGroupReplyE(action.ChatID, action.ReplyToMessageID, action.UserID, action.Text)
	case action.Quote:
		err = r.obClient.SendGroupReplyE(action.ChatID, action.ReplyToMessageID, "", action.Text)
	default:
		err = r.obClient.SendGroupMessageE(action.ChatID, action.Text)
	}
	if err == nil {
		entry.Sent = true
		entry.SendOK = true
		return entry, nil
	}
	var sendErr *onebot.SendError
	if errors.As(err, &sendErr) && sendErr.Uncertain {
		entry.Sent = true
		entry.SendOK = false
		logutil.Warn("发送结果不确定：按已发记录，不重复发送", "action", action.ID, "err", err)
		return entry, nil
	}
	entry.Sent = false
	entry.SendOK = false
	return entry, err
}
