package workspace

import (
	"good-review-master/speech"
	"good-review-master/state"
)

// sendLedger 把 router 的发送台账接口落到本工作空间的 state.Scope 上。
type sendLedger struct {
	scope *state.Scope
}

func (l sendLedger) Load(actionID string) (speech.LedgerEntry, bool, error) {
	if l.scope == nil {
		return speech.LedgerEntry{}, false, nil
	}
	record, ok, err := l.scope.LoadSendRecord(actionID)
	if err != nil || !ok {
		return speech.LedgerEntry{}, ok, err
	}
	return speech.LedgerEntry{
		ActionID: record.ActionID, MessageID: record.MessageID,
		Text: record.Text, Sent: record.Sent, SendOK: record.SendOK,
	}, true, nil
}

func (l sendLedger) Mark(entry speech.LedgerEntry) {
	if l.scope == nil {
		return
	}
	l.scope.MarkSend(state.SendRecord{
		ActionID: entry.ActionID, MessageID: entry.MessageID,
		Text: entry.Text, Sent: entry.Sent, SendOK: entry.SendOK,
	})
}
