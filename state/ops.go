package state

// send_ledger / alerts / settings / workspace_profile 列表：出站台账、异常现场、
// 全局设置，以及重启时"有哪些工作空间要恢复"。这几张表都是给运维和面板看的。

import (
	"encoding/json"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SendRecord 发送台账的一行：动作编号是幂等键。
type SendRecord struct {
	// ID 是自增主键；(workspace_id, action_id) 是业务键（表上是 UNIQUE）。
	ID          int64  `gorm:"column:id;primaryKey;autoIncrement"`
	WorkspaceID string `gorm:"column:workspace_id"`
	ActionID    string `gorm:"column:action_id"`
	MessageID   string `gorm:"column:message_id"`
	Text        string `gorm:"column:text"`
	Sent        bool   `gorm:"column:sent"`
	SendOK      bool   `gorm:"column:send_ok"`
	UpdatedAt   int64  `gorm:"column:updated_at"`
}

func (SendRecord) TableName() string { return "send_ledger" }

// Alert 放弃回复等异常现场。Detail 是完整 JSON。
type Alert struct {
	ID          int64  `gorm:"column:id;primaryKey;autoIncrement"`
	WorkspaceID string `gorm:"column:workspace_id"`
	Kind        string `gorm:"column:kind"`
	Detail      string `gorm:"column:detail"`
	CreatedAt   int64  `gorm:"column:created_at"`
}

func (Alert) TableName() string { return "alerts" }

// Setting 全局运行时设置。Key 是开放集合，所以保留 KV；画像那种固定字段不用它。
type Setting struct {
	// ID 是自增主键；key 是业务键（表上是 UNIQUE），upsert 的冲突目标按它写。
	ID        int64  `gorm:"column:id;primaryKey;autoIncrement"`
	Key       string `gorm:"column:key"`
	Value     string `gorm:"column:value"`
	UpdatedAt int64  `gorm:"column:updated_at"`
}

func (Setting) TableName() string { return "settings" }

// WorkspaceMeta 是重启恢复工作空间所需的最小信息。
type WorkspaceMeta struct {
	ID          string
	Platform    string
	AccountKind string
	Account     string
	Name        string
}

// Workspaces 返回库里记录过的全部工作空间（来自 workspace_profile）。
func (s *Store) Workspaces() ([]WorkspaceMeta, error) {
	if s == nil {
		return nil, nil
	}
	var profiles []WorkspaceProfile
	if err := s.db.Find(&profiles).Error; err != nil {
		return nil, err
	}
	out := make([]WorkspaceMeta, 0, len(profiles))
	for _, profile := range profiles {
		out = append(out, WorkspaceMeta{
			ID: profile.WorkspaceID, Platform: profile.Platform,
			AccountKind: profile.AccountKind, Account: profile.Account, Name: profile.Name,
		})
	}
	return out, nil
}

// LoadSendRecord 读取一个动作的发送台账。
func (s *Scope) LoadSendRecord(actionID string) (SendRecord, bool, error) {
	if s == nil || s.store == nil || actionID == "" {
		return SendRecord{}, false, nil
	}
	var record SendRecord
	err := s.store.db.Where("workspace_id = ? AND action_id = ?", s.workspaceID, actionID).
		First(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return SendRecord{}, false, nil
	}
	if err != nil {
		return SendRecord{}, false, err
	}
	return record, true, nil
}

// MarkSend 写入/更新发送台账。
func (s *Scope) MarkSend(record SendRecord) {
	if s == nil || s.store == nil || record.ActionID == "" || s.store.closed.Load() {
		return
	}
	record.WorkspaceID = s.workspaceID
	record.UpdatedAt = time.Now().Unix()
	// 冲突目标必须显式指定业务键：主键是自增 id 后，默认目标会变成 id，等于只插不更新。
	if err := s.store.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "workspace_id"}, {Name: "action_id"}},
		UpdateAll: true,
	}).Create(&record).Error; err != nil {
		s.store.noteFailure("保存发送台账", err)
	}
}

// SaveAlert 写入一条异常现场。
func (s *Store) SaveAlert(alert Alert) {
	if s == nil || alert.Kind == "" {
		return
	}
	if alert.CreatedAt == 0 {
		alert.CreatedAt = time.Now().Unix()
	}
	if err := s.db.Create(&alert).Error; err != nil {
		s.noteFailure("保存报警", err)
	}
}

// LoadAlerts 读最近的报警，按时间倒序，同时给出总条数。
//
// total 不受 limit 影响：页面上要写「最近 20 条 / 共 137 条」，
// 光看列表长度分不清是攒了很多条还是刚好就是这个数。
// count 和取列表都走 idx_alerts_created，两条都是索引扫描。
func (s *Store) LoadAlerts(limit int) ([]Alert, int64, error) {
	if s == nil || s.closed.Load() {
		return nil, 0, nil
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}

	var total int64
	if err := s.db.Model(&Alert{}).Count(&total).Error; err != nil {
		s.noteFailure("统计报警", err)
		return nil, 0, err
	}

	var list []Alert
	if err := s.db.Order("created_at DESC, id DESC").Limit(limit).Find(&list).Error; err != nil {
		s.noteFailure("读取报警", err)
		return nil, 0, err
	}
	return list, total, nil
}

// LoadSettings 读取全局运行时设置；value 是 JSON。
func (s *Store) LoadSettings() (map[string]json.RawMessage, error) {
	if s == nil {
		return nil, nil
	}
	var rows []Setting
	if err := s.db.Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]json.RawMessage, len(rows))
	for _, row := range rows {
		out[row.Key] = json.RawMessage(row.Value)
	}
	return out, nil
}

// SaveSetting 写入全局运行时设置；value 会被编码成 JSON。
func (s *Store) SaveSetting(key string, value any) error {
	if s == nil || key == "" {
		return nil
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	row := Setting{Key: key, Value: string(payload), UpdatedAt: time.Now().Unix()}
	// 冲突目标是 key，不是自增主键 id：写错的话每次都会新插一行，设置越存越多。
	return s.db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "key"}},
		UpdateAll: true,
	}).Create(&row).Error
}
