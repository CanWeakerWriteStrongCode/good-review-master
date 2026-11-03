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
)

// WorkspaceProfile 是工作空间画像：常用字段建列，其余进 Extra JSON。
type WorkspaceProfile struct {
	// ID 是自增主键；workspace_id 是业务键（表上是 UNIQUE），upsert 的冲突目标按它写。
	ID             int64  `gorm:"column:id;primaryKey;autoIncrement"`
	WorkspaceID    string `gorm:"column:workspace_id"`
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
// 切换人格要求先命中一个带人格的关键字，那时群里通常已经有消息、groups 行
// 早建出来了；但"一条消息都没存过"的极端情况也不该把人格丢掉，所以要补行。
// updated_at 写 0，表示档案本身还没写过。
func (s *Scope) SaveGroupPersona(groupID, keyword string) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return
	}
	// 先 UPDATE，没命中才补行：upsert 即使只走更新分支也会分配自增 rowid，
	// 每切一次人格就把 groups.id 推高一个（详见 schema.go 的 createGroupStatTriggers）。
	res := s.store.db.Exec(`UPDATE groups SET persona_keyword = ?
			WHERE workspace_id = ? AND group_id = ?`,
		keyword, s.workspaceID, groupID)
	if res.Error == nil && res.RowsAffected == 0 {
		// 群里一条消息都没存过：补一行。仍带 ON CONFLICT，只防并发下别人刚插过。
		res = s.store.db.Exec(`INSERT INTO groups
				(workspace_id, group_id, type, persona_keyword, extra, updated_at)
			VALUES (?, ?, CASE WHEN ? LIKE 'private:%' THEN 'private' ELSE 'group' END, ?, '{}', 0)
			ON CONFLICT(workspace_id, group_id) DO UPDATE SET
				persona_keyword = excluded.persona_keyword`,
			s.workspaceID, groupID, groupID, keyword)
	}
	if res.Error != nil {
		s.store.noteFailure("保存群人格", res.Error)
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
	// 先 UPDATE、没命中才 INSERT：upsert 即使走更新分支也会分配自增 rowid，
	// 每同步一次画像就把 workspace_profile.id 推高一个（详见 schema.go 的
	// createGroupStatTriggers）。created_at 只在插入时写。
	res := s.store.db.Exec(`UPDATE workspace_profile SET
			platform = ?, account_kind = ?, account = ?, name = ?, impl = ?,
			identity_scheme = ?, identity_scope = ?, extra = ?, updated_at = ?
		WHERE workspace_id = ?`,
		profile.Platform, profile.AccountKind, profile.Account, profile.Name, profile.Impl,
		profile.IdentityScheme, profile.IdentityScope, profile.Extra, profile.UpdatedAt,
		profile.WorkspaceID)
	if res.Error == nil && res.RowsAffected == 0 {
		res = s.store.db.Exec(`INSERT INTO workspace_profile
				(workspace_id, platform, account_kind, account, name, impl,
				 identity_scheme, identity_scope, extra, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			profile.WorkspaceID, profile.Platform, profile.AccountKind, profile.Account,
			profile.Name, profile.Impl, profile.IdentityScheme, profile.IdentityScope,
			profile.Extra, profile.CreatedAt, profile.UpdatedAt)
	}
	return res.Error
}
