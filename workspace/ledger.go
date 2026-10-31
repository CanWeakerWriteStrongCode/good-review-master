package workspace

import (
	"good-review-master/router"
	"good-review-master/workspace/state"
)

// sendLedger 把 router 的发送台账接口落到本工作空间的 state.Scope 上。
type sendLedger struct {
	scope *state.Scope
}

func (l sendLedger) Load(actionID string) (router.LedgerEntry, bool, error) {
	if l.scope == nil {
		return router.LedgerEntry{}, false, nil
	}
	record, ok, err := l.scope.LoadSendRecord(actionID)
	if err != nil || !ok {
		return router.LedgerEntry{}, ok, err
	}
	return router.LedgerEntry{
		ActionID: record.ActionID, MessageID: record.MessageID,
		Text: record.Text, Sent: record.Sent, SendOK: record.SendOK,
	}, true, nil
}

func (l sendLedger) Mark(entry router.LedgerEntry) {
	if l.scope == nil {
		return
	}
	l.scope.MarkSend(state.SendRecord{
		ActionID: entry.ActionID, MessageID: entry.MessageID,
		Text: entry.Text, Sent: entry.Sent, SendOK: entry.SendOK,
	})
}
