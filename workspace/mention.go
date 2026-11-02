package workspace

// 历史消息里的 @ 占位回填。
//
// 入站渲染时被 @ 的人还没名字，正文会写成 @用户(openid)；名字随后从 sender 透传
// 或 get_group_member_info 学到，这里把正文里的占位换成真名。
// 待办就写在正文里：mentions_resolved = 0 的行还会被扫到，换完自动出列，
// 所以不需要任务表，也不需要抢占——多个进程同时跑只是重复干活，结果一样。

import (
	"context"

	"good-review-master/conversation"
	"good-review-master/logutil"
)

const (
	// mentionBackfillBatch 一轮改多少行。写锁按批释放，别挡住入站消息落库。
	mentionBackfillBatch = 200
	// mentionBackfillRounds 一次唤醒最多连跑几轮，防止超长历史把协程占住。
	// 没跑完的下次学到新名字、或下次启动时会接着来。
	mentionBackfillRounds = 20
)

// scheduleMentionBackfill 按群去抖：同一个群同时只有一个回填协程。
//
// 一条消息进来可能一口气补上好几个名字（发送者 + 几个 @ 对象），
// 每次学习都起协程会重复扫同一批行，这里合成一遍；正在跑的时候又有人叫，
// 就记一笔“跑完再来一遍”，不会把这期间新落库的行漏掉。
func (w *Workspace) scheduleMentionBackfill(groupID string) {
	if w.State == nil || groupID == "" {
		return
	}
	w.mentionMu.Lock()
	if w.mentionQueued == nil {
		w.mentionQueued = make(map[string]bool)
		w.mentionAgain = make(map[string]bool)
	}
	if w.mentionQueued[groupID] {
		w.mentionAgain[groupID] = true
		w.mentionMu.Unlock()
		return
	}
	w.mentionQueued[groupID] = true
	w.mentionMu.Unlock()

	ctx := w.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		for {
			w.mentionMu.Lock()
			w.mentionAgain[groupID] = false
			w.mentionMu.Unlock()

			for round := 0; round < mentionBackfillRounds && ctx.Err() == nil; round++ {
				progressed, more := w.rewriteMentions(ctx, groupID)
				if !progressed || !more {
					break
				}
			}

			w.mentionMu.Lock()
			again := w.mentionAgain[groupID] && ctx.Err() == nil
			if !again {
				delete(w.mentionQueued, groupID)
			}
			w.mentionMu.Unlock()
			if !again {
				return
			}
		}
	}()
}

// rewriteMentions 跑一批：取待办 → 一次查名字 → 逐条改写。
//
// 返回这一批有没有真的改到东西，以及后面可能还有没有（取满了说明还有）。
// 一个名字都没学到时 progressed 为 false，协程就此收手，等下一次学习再触发。
func (w *Workspace) rewriteMentions(ctx context.Context, groupID string) (progressed, more bool) {
	rows, err := w.State.PendingMentionRows(groupID, mentionBackfillBatch)
	if err != nil {
		logutil.Warn("读取待回填的 @ 占位失败", "workspace", w.ID, "group", groupID, "err", err)
		return false, false
	}
	if len(rows) == 0 {
		return false, false
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, conversation.MentionIDs(row.Content)...)
	}
	// 内存目录优先，缺的按群批量查一次 members 表。
	names := w.ResolveNames(groupID, ids)

	changed := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			break
		}
		next := conversation.ResolveMentions(row.Content, names)
		resolved := 0
		switch {
		case !conversation.HasMentionPlaceholder(next):
			// 全换完了，这条出列。
			resolved = 1
		case conversation.HasMentionPlaceholder(row.Content) && len(conversation.MentionIDs(row.Content)) == 0:
			// 有 "@用户(" 但不是 "@用户(完整id)" 的形态：正文被截断，或者谁手打的。
			// 这种永远换不掉，直接摘出待办，否则每次回填都白跑一遍。
			resolved = 1
		}
		if next == row.Content && resolved == 0 {
			continue // 这批里没有一个人有名字，等下次
		}
		ok, err := w.State.RewriteMention(row.ID, row.Content, next, resolved)
		if err != nil {
			logutil.Warn("回填 @ 占位失败", "workspace", w.ID, "group", groupID, "row", row.ID, "err", err)
			continue
		}
		if ok {
			changed++
		}
	}
	if changed > 0 {
		logutil.Debug("已回填历史消息里的 @ 占位", "workspace", w.ID, "group", groupID, "rows", changed)
	}
	return changed > 0, len(rows) == mentionBackfillBatch
}

// backfillAllMentions 启动时补一遍：上次可能停在"名字已学到、正文还没换"的位置就退出了。
func (w *Workspace) backfillAllMentions() {
	if w.State == nil {
		return
	}
	groups, err := w.State.PendingMentionGroups()
	if err != nil {
		logutil.Warn("读取待回填 @ 的群失败", "workspace", w.ID, "err", err)
		return
	}
	for _, groupID := range groups {
		w.scheduleMentionBackfill(groupID)
	}
}
