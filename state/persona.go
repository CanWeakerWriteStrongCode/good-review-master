package state

// persona.go：这个群用的哪套人格，这个账号是谁。
//
// 群人格存在 groups.persona_keyword（v13 起从 group_personas 表并进来）；
// 工作空间画像仍是单独一张表。

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// WorkspaceProfile 是工作空间画像：常用字段建列，其余进 Extra JSON。
type WorkspaceProfile struct {
	WorkspaceID    string `gorm:"column:workspace_id;primaryKey"`
	Platform       string `gorm:"column:platform"`
	AccountKind    string `gorm:"column:account_kind"`
	Account        string `gorm:"column:account"`
	Name           string `gorm:"column:name"`
	Impl           string `gorm:"column:impl"`
	IdentityScheme string `gorm:"column:identity_scheme"`
	IdentityScope  string `gorm:"column:identity_scope"`
	Extra          string `gorm:"column:extra"`
	CreatedAt      int64  `gorm:"column:created_at"`
	UpdatedAt      int64  `gorm:"column:updated_at"`
}

func (WorkspaceProfile) TableName() string { return "workspace_profile" }

// SaveGroupPersona 记录某群当前人格的源关键字（写 groups.persona_keyword）。
//
// 用 UPSERT 而不是 UPDATE：切换人格要求先命中一个带人格的关键字，那时群里
// 通常已经有消息、groups 行早建出来了；但「一条消息都没存过」的极端情况
// 也不该把人格丢掉。updated_at 写 0，表示档案本身还没写过。
func (s *Scope) SaveGroupPersona(groupID, keyword string) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return
	}
	if err := s.store.db.Exec(`INSERT INTO groups
			(workspace_id, group_id, type, persona_keyword, extra, updated_at)
		VALUES (?, ?, CASE WHEN ? LIKE 'private:%' THEN 'private' ELSE 'group' END, ?, '{}', 0)
		ON CONFLICT(workspace_id, group_id) DO UPDATE SET
			persona_keyword = excluded.persona_keyword`,
		s.workspaceID, groupID, groupID, keyword).Error; err != nil {
		s.store.noteFailure("保存群人格", err)
	}
}

// DeleteGroupPersona 清除某群人格（#取消人格）。群档案行保留，只把关键字清空。
func (s *Scope) DeleteGroupPersona(groupID string) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return
	}
	if err := s.store.db.Exec(`UPDATE groups SET persona_keyword = ''
		WHERE workspace_id = ? AND group_id = ?`, s.workspaceID, groupID).Error; err != nil {
		s.store.noteFailure("删除群人格", err)
	}
}

// LoadGroupPersonas 返回 group → 人格关键字（只取设过人格的行）。
func (s *Scope) LoadGroupPersonas() (map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	var rows []struct {
		GroupID string `gorm:"column:group_id"`
		Keyword string `gorm:"column:persona_keyword"`
	}
	if err := s.store.db.Raw(`SELECT group_id, persona_keyword FROM groups
		WHERE workspace_id = ? AND persona_keyword <> ''`, s.workspaceID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.GroupID] = row.Keyword
	}
	return out, nil
}

// LoadProfile 读取工作空间画像。第二个返回值表示记录是否存在。
func (s *Scope) LoadProfile() (WorkspaceProfile, bool, error) {
	if s == nil || s.store == nil {
		return WorkspaceProfile{}, false, nil
	}
	var profile WorkspaceProfile
	err := s.store.db.Where("workspace_id = ?", s.workspaceID).First(&profile).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return WorkspaceProfile{}, false, nil
	}
	if err != nil {
		return WorkspaceProfile{}, false, err
	}
	return profile, true, nil
}

// SaveProfile 写入/更新画像。CreatedAt 只在首次写入时设置。
func (s *Scope) SaveProfile(profile WorkspaceProfile) error {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return nil
	}
	profile.WorkspaceID = s.workspaceID
	now := time.Now().Unix()
	if profile.CreatedAt == 0 {
		profile.CreatedAt = now
	}
	profile.UpdatedAt = now
	if strings.TrimSpace(profile.Extra) == "" {
		profile.Extra = "{}"
	}
	return s.store.db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "workspace_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"platform", "account_kind", "account", "name", "impl",
			"identity_scheme", "identity_scope", "extra", "updated_at",
		}),
	}).Create(&profile).Error
}
