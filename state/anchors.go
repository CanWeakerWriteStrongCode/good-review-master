package state

// anchors 表：每个群「上次发出去的那个窗口」的首尾两条消息。
// 这两个位置是前缀缓存的命门——知道上次从哪条发到哪条，这次才能接着往后加。

import (
	"time"

	"good-review-master/conversation"

	"gorm.io/gorm/clause"
)

// anchorRow 每个群「上次发出去的那个窗口」的首尾两条消息。
//
// 改名带上 window_ 前缀是因为 start/last_sent 看不出说的是"窗口的起点和终点"，
// 光看列名会以为是时间或者队列位置。
type anchorRow struct {
	// ID 是自增主键；(workspace_id, group_id) 是业务键（表上是 UNIQUE）。
	ID            int64  `gorm:"column:id;primaryKey;autoIncrement"`
	WorkspaceID   string `gorm:"column:workspace_id"`
	GroupID       string `gorm:"column:group_id"`
	WindowFirstID string `gorm:"column:window_first_id"` // 上次窗口第一条消息的编号（messages.id）
	WindowLastID  string `gorm:"column:window_last_id"`  // 上次窗口最后一条消息的编号，之后的就是"新增"
	UpdatedAt     int64  `gorm:"column:updated_at"`      // 写入时刻（Unix 秒），同时用作缓存是否过期的基准
}

func (anchorRow) TableName() string { return "anchors" }

// LoadAnchors 返回本工作空间全部群的选窗锚点。
// updated_at 一并读回成 At：它是"上次真正把窗口发出去"的时刻，判断缓存是否过期靠它。
func (s *Scope) LoadAnchors() (map[string]conversation.LLMAnchor, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	var rows []anchorRow
	if err := s.store.db.Where("workspace_id = ?", s.workspaceID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]conversation.LLMAnchor, len(rows))
	for _, row := range rows {
		anchor := conversation.LLMAnchor{Start: row.WindowFirstID, LastSent: row.WindowLastID}
		// 0 表示这行是更早的版本写的、没记时间，留零值让 Expired 按"未知"处理，而不是当成 1970 年。
		if row.UpdatedAt > 0 {
			anchor.At = time.Unix(row.UpdatedAt, 0)
		}
		out[row.GroupID] = anchor
	}
	return out, nil
}

// SaveAnchor 记录本群"上次发出去的窗口是哪一段"。锚点变更很少，直接同步写。
func (s *Scope) SaveAnchor(groupID string, anchor conversation.LLMAnchor) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return
	}
	at := anchor.At
	if at.IsZero() {
		at = time.Now()
	}
	row := anchorRow{
		WorkspaceID:   s.workspaceID,
		GroupID:       groupID,
		WindowFirstID: anchor.Start,
		WindowLastID:  anchor.LastSent,
		UpdatedAt:     at.Unix(),
	}
	// 冲突目标是 (workspace_id, group_id)：主键换成自增 id 后不显式指定就会打到 id 上。
	if err := s.store.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "workspace_id"}, {Name: "group_id"}},
		UpdateAll: true,
	}).Create(&row).Error; err != nil {
		s.store.noteFailure("保存锚点", err)
	}
}
