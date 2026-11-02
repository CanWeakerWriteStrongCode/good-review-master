package state

// group_personas / workspace_profile：这个群用的哪套人格，这个账号是谁。

import (
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type groupPersonaRow struct {
	WorkspaceID string `gorm:"column:workspace_id;primaryKey"`
	GroupID     string `gorm:"column:group_id;primaryKey"`
	Keyword     string `gorm:"column:keyword"`
}

func (groupPersonaRow) TableName() string { return "group_personas" }

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

// SaveGroupPersona 记录某群当前人格的源关键字。
func (s *Scope) SaveGroupPersona(groupID, keyword string) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return
	}
	row := groupPersonaRow{WorkspaceID: s.workspaceID, GroupID: groupID, Keyword: keyword}
	if err := s.store.db.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error; err != nil {
		s.store.noteFailure("保存群人格", err)
	}
}

// DeleteGroupPersona 清除某群人格。
func (s *Scope) DeleteGroupPersona(groupID string) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return
	}
	if err := s.store.db.Where("workspace_id = ? AND group_id = ?", s.workspaceID, groupID).
		Delete(&groupPersonaRow{}).Error; err != nil {
		s.store.noteFailure("删除群人格", err)
	}
}

// LoadGroupPersonas 返回 group → 人格关键字。
func (s *Scope) LoadGroupPersonas() (map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	var rows []groupPersonaRow
	if err := s.store.db.Where("workspace_id = ?", s.workspaceID).Find(&rows).Error; err != nil {
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
