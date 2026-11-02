package state

// members / groups / member_info_history / group_info_history：
// 这个群叫什么、群里谁叫什么、机器人自己的号是哪个（groups.self_user_id）。
//
// 读路径先走 Ristretto（key 见 memberKVKey / groupKVKey），miss 再回源。
// 写路径先写历史再更新当前值，不依赖跨表事务；历史表的 source_message_id
// 唯一键负责防重试重复写。

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
)

// 缓存 TTL：成员/群信息会被平台改写，不能永久缓存。
const (
	memberCacheTTL = 7 * 24 * time.Hour
	groupCacheTTL  = 7 * 24 * time.Hour
)

type memberRow struct {
	WorkspaceID string `gorm:"column:workspace_id;primaryKey"`
	GroupID     string `gorm:"column:group_id;primaryKey"`
	UserID      string `gorm:"column:user_id;primaryKey"`
	Name        string `gorm:"column:name"`
	DisplayName string `gorm:"column:display_name"`
	Extra       string `gorm:"column:extra"`
	UpdatedAt   int64  `gorm:"column:updated_at"`
}

func (memberRow) TableName() string { return "members" }

type groupRow struct {
	WorkspaceID string `gorm:"column:workspace_id;primaryKey"`
	GroupID     string `gorm:"column:group_id;primaryKey"`
	Name        string `gorm:"column:name"`
	// Type 会话类型：group = 群聊，private = 私聊。
	// 私聊在库里的 group_id 是 private:<openid>，和群号一样按会话存。
	Type string `gorm:"column:type"`
	// SelfUserID 本群学到的机器人 user_id（QQ 官方是群作用域 openid）。
	// 用来在 members 里认出哪一行是机器人自己；哨兵值不可用于 @ 比较。
	SelfUserID string `gorm:"column:self_user_id"`
	// PersonaKeyword 本群当前人格的源关键字（#切换人格 设置，空串表示没有）。
	// 只由 persona.go 读写，这里列出来是为了让结构体和表列对得上。
	PersonaKeyword string `gorm:"column:persona_keyword"`
	MessageCount   int    `gorm:"column:message_count"`
	MemberCount    int    `gorm:"column:member_count"`
	LastTS         int64  `gorm:"column:last_ts"`
	Extra          string `gorm:"column:extra"`
	UpdatedAt      int64  `gorm:"column:updated_at"`
}

func (groupRow) TableName() string { return "groups" }

// MemberInfoHistory 成员信息变更历史。通用列只放常用查询字段，其余进 Extra。
type MemberInfoHistory struct {
	ID              int64  `gorm:"column:id;primaryKey;autoIncrement"`
	WorkspaceID     string `gorm:"column:workspace_id"`
	GroupID         string `gorm:"column:group_id"`
	UserID          string `gorm:"column:user_id"`
	Name            string `gorm:"column:name"`
	DisplayName     string `gorm:"column:display_name"`
	Source          string `gorm:"column:source"`
	SourceMessageID string `gorm:"column:source_message_id"`
	Extra           string `gorm:"column:extra"`
	ChangedAt       int64  `gorm:"column:changed_at"`
}

func (MemberInfoHistory) TableName() string { return "member_info_history" }

// GroupInfoHistory 群信息变更历史。
type GroupInfoHistory struct {
	ID              int64  `gorm:"column:id;primaryKey;autoIncrement"`
	WorkspaceID     string `gorm:"column:workspace_id"`
	GroupID         string `gorm:"column:group_id"`
	Name            string `gorm:"column:name"`
	Source          string `gorm:"column:source"`
	SourceMessageID string `gorm:"column:source_message_id"`
	Extra           string `gorm:"column:extra"`
	ChangedAt       int64  `gorm:"column:changed_at"`
}

func (GroupInfoHistory) TableName() string { return "group_info_history" }

// SaveIdentity 记录本群学到的机器人 user_id（可能是哨兵）。
//
// 存在 groups.self_user_id 里：身份和会话档案都是每会话一行，没必要分两张表。
// updated_at 写 0，表示“档案还没写过”，不会把档案的更新时间冲掉。
func (s *Scope) SaveIdentity(groupID, selfUserID string) {
	if s == nil || s.store == nil || selfUserID == "" || s.store.closed.Load() {
		return
	}
	if err := s.store.db.Exec(`INSERT INTO groups
			(workspace_id, group_id, type, self_user_id, extra, updated_at)
		VALUES (?, ?, CASE WHEN ? LIKE 'private:%' THEN 'private' ELSE 'group' END, ?, '{}', 0)
		ON CONFLICT(workspace_id, group_id) DO UPDATE SET
			self_user_id = excluded.self_user_id`,
		s.workspaceID, groupID, groupID, selfUserID).Error; err != nil {
		s.store.noteFailure("保存身份", err)
	}
}

// LoadIdentities 返回各会话学到的机器人 user_id（读 groups 表）。
func (s *Scope) LoadIdentities() (map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	var rows []struct {
		GroupID    string `gorm:"column:group_id"`
		SelfUserID string `gorm:"column:self_user_id"`
	}
	if err := s.store.db.Raw(`SELECT group_id, self_user_id FROM groups
		WHERE workspace_id = ? AND self_user_id <> ''`, s.workspaceID).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.GroupID] = row.SelfUserID
	}
	return out, nil
}

// MemberInfo 一次成员信息写入。Source/SourceMessageID 用于历史去重与回溯。
type MemberInfo struct {
	GroupID         string
	UserID          string
	Name            string
	DisplayName     string
	Source          string
	SourceMessageID string
	Extra           string
}

func memberKVKey(workspaceID, groupID, userID string) string {
	return fmt.Sprintf("ws:%s:member:%s:%s", workspaceID, groupID, userID)
}

// SaveMemberInfo 更新成员当前值；信息确实变化时追加一行历史。
// 先写历史、再更新当前值，不依赖跨表事务；source_message_id 唯一键防重试重复写。
func (s *Scope) SaveMemberInfo(info MemberInfo) (bool, error) {
	if s == nil || s.store == nil || info.UserID == "" || s.store.closed.Load() {
		return false, nil
	}
	extra := strings.TrimSpace(info.Extra)
	if extra == "" {
		extra = "{}"
	}
	var current memberRow
	err := s.store.db.Where("workspace_id = ? AND group_id = ? AND user_id = ?",
		s.workspaceID, info.GroupID, info.UserID).First(&current).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		s.store.noteFailure("读取成员", err)
		return false, err
	}

	name, display := info.Name, info.DisplayName
	if name == "" {
		name = current.Name
	}
	if display == "" {
		display = current.DisplayName
	}
	extraChanged := current.UpdatedAt == 0 || (extra != "{}" && extra != current.Extra)
	changed := current.UpdatedAt == 0 || name != current.Name || display != current.DisplayName || extraChanged
	if !changed {
		return false, nil
	}

	now := time.Now().Unix()
	// 历史先写：即使随后更新当前值失败，也不会漏记一次变更。
	if info.Source != "" || info.SourceMessageID != "" {
		if err := s.store.db.Exec(`INSERT INTO member_info_history
			(workspace_id, group_id, user_id, name, display_name, source, source_message_id, extra, changed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING`,
			s.workspaceID, info.GroupID, info.UserID, name, display,
			info.Source, info.SourceMessageID, extra, now).Error; err != nil {
			s.store.noteFailure("写成员历史", err)
		}
	}
	if err := s.store.db.Exec(`INSERT INTO members
		(workspace_id, group_id, user_id, name, display_name, extra, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id, group_id, user_id) DO UPDATE SET
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE members.name END,
			display_name = CASE WHEN excluded.display_name <> '' THEN excluded.display_name ELSE members.display_name END,
			extra = CASE WHEN excluded.extra <> '{}' THEN excluded.extra ELSE members.extra END,
			updated_at = excluded.updated_at`,
		s.workspaceID, info.GroupID, info.UserID, name, display, extra, now).Error; err != nil {
		s.store.noteFailure("保存成员名", err)
		return false, err
	}
	if display == "" {
		display = name
	}
	if display != "" {
		_ = s.store.kv.Set(nil, memberKVKey(s.workspaceID, info.GroupID, info.UserID), []byte(display), memberCacheTTL)
	}
	return true, nil
}

// SaveMember 兼容旧调用：没有触发消息编号，历史只按信息变化追加。
func (s *Scope) SaveMember(groupID, userID, name, displayName string) {
	_, _ = s.SaveMemberInfo(MemberInfo{
		GroupID: groupID, UserID: userID, Name: name, DisplayName: displayName,
		Source: "member_info",
	})
}

// RenameSelfMember 把「机器人自己」在某个群的成员行改成新名字，并追加 self_name 历史。
func (s *Scope) RenameSelfMember(groupID, userID, oldName, newName string) {
	if s == nil || s.store == nil || groupID == "" || userID == "" || newName == "" || s.store.closed.Load() {
		return
	}
	var current memberRow
	err := s.store.db.Where("workspace_id = ? AND group_id = ? AND user_id = ?",
		s.workspaceID, groupID, userID).First(&current).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		s.store.noteFailure("读取成员", err)
		return
	}
	display := current.DisplayName
	if display == "" || display == oldName {
		display = newName
	}
	if current.Name == newName && current.DisplayName == display {
		return
	}
	_, _ = s.SaveMemberInfo(MemberInfo{
		GroupID: groupID, UserID: userID, Name: newName, DisplayName: display,
		Source: "self_name",
	})
}

// LoadMembers 返回 group → user → 显示名。
func (s *Scope) LoadMembers() (map[string]map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	var rows []memberRow
	if err := s.store.db.Where("workspace_id = ?", s.workspaceID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]map[string]string)
	for _, row := range rows {
		if out[row.GroupID] == nil {
			out[row.GroupID] = make(map[string]string)
		}
		display := row.DisplayName
		if display == "" {
			display = row.Name
		}
		out[row.GroupID][row.UserID] = display
	}
	return out, nil
}

// LookupMemberNames 按群批量查成员显示名：先读 KV，miss 的按批回源并回填缓存。
func (s *Scope) LookupMemberNames(groupID string, userIDs []string) map[string]string {
	if s == nil || s.store == nil || groupID == "" || len(userIDs) == 0 || s.store.closed.Load() {
		return nil
	}
	out := make(map[string]string, len(userIDs))
	keys := make([]string, 0, len(userIDs))
	byKey := make(map[string]string, len(userIDs))
	for _, id := range userIDs {
		if id == "" {
			continue
		}
		key := memberKVKey(s.workspaceID, groupID, id)
		keys = append(keys, key)
		byKey[key] = id
	}
	cached, _ := s.store.kv.GetMany(nil, keys)
	missing := make([]string, 0, len(userIDs))
	for _, key := range keys {
		if value, ok := cached[key]; ok && len(value) > 0 {
			out[byKey[key]] = string(value)
			continue
		}
		missing = append(missing, byKey[key])
	}
	if len(missing) == 0 {
		return out
	}

	const chunk = 400
	for start := 0; start < len(missing); start += chunk {
		end := min(start+chunk, len(missing))
		args := make([]any, 0, end-start+2)
		args = append(args, s.workspaceID, groupID)
		holders := make([]string, 0, end-start)
		for _, id := range missing[start:end] {
			args = append(args, id)
			holders = append(holders, "?")
		}
		query := `SELECT user_id, name, display_name FROM members
			WHERE workspace_id = ? AND group_id = ? AND user_id IN (` + strings.Join(holders, ",") + `)`
		rows, err := s.store.db.Raw(query, args...).Rows()
		if err != nil {
			s.store.noteFailure("批量查询成员名", err)
			return out
		}
		for rows.Next() {
			var userID, name, display string
			if err := rows.Scan(&userID, &name, &display); err != nil {
				s.store.noteFailure("批量查询成员名", err)
				break
			}
			value := display
			if value == "" {
				value = name
			}
			if value == "" {
				continue
			}
			out[userID] = value
			_ = s.store.kv.Set(nil, memberKVKey(s.workspaceID, groupID, userID), []byte(value), memberCacheTTL)
		}
		if err := rows.Err(); err != nil {
			s.store.noteFailure("批量查询成员名", err)
		}
		_ = rows.Close()
	}
	return out
}

func groupKVKey(workspaceID, groupID string) string {
	return fmt.Sprintf("ws:%s:group:%s", workspaceID, groupID)
}

// SaveGroupInfo 更新群档案；群名或 extra 变化时追加历史。
func (s *Scope) SaveGroupInfo(groupID, name, source, sourceMessageID, extra string) (bool, error) {
	if s == nil || s.store == nil || groupID == "" || s.store.closed.Load() {
		return false, nil
	}
	if strings.TrimSpace(extra) == "" {
		extra = "{}"
	}
	var current groupRow
	err := s.store.db.Where("workspace_id = ? AND group_id = ?", s.workspaceID, groupID).First(&current).Error
	exists := err == nil
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		s.store.noteFailure("读取群档案", err)
		return false, err
	}
	effectiveName := name
	if effectiveName == "" {
		effectiveName = current.Name
	}
	// 行可能已经被身份写入建出来了（self_user_id 先到、群名后到），
	// 所以“是不是新行”看这次查询找没找到，不看 updated_at。
	extraChanged := !exists || (extra != "{}" && extra != current.Extra)
	changed := !exists || effectiveName != current.Name || extraChanged
	if !changed {
		return false, nil
	}
	now := time.Now().Unix()
	if source != "" || sourceMessageID != "" {
		if err := s.store.db.Exec(`INSERT INTO group_info_history
			(workspace_id, group_id, name, source, source_message_id, extra, changed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING`,
			s.workspaceID, groupID, effectiveName, source, sourceMessageID, extra, now).Error; err != nil {
			s.store.noteFailure("写群历史", err)
		}
	}
	if err := s.store.db.Exec(`INSERT INTO groups (workspace_id, group_id, name, extra, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id, group_id) DO UPDATE SET
			name = CASE WHEN excluded.name <> '' THEN excluded.name ELSE groups.name END,
			extra = CASE WHEN excluded.extra <> '{}' THEN excluded.extra ELSE groups.extra END,
			updated_at = excluded.updated_at`,
		s.workspaceID, groupID, effectiveName, extra, now).Error; err != nil {
		s.store.noteFailure("保存群档案", err)
		return false, err
	}
	if effectiveName != "" {
		_ = s.store.kv.Set(nil, groupKVKey(s.workspaceID, groupID), []byte(effectiveName), groupCacheTTL)
	}
	return true, nil
}

// SaveGroup 兼容旧调用。
func (s *Scope) SaveGroup(groupID, name string) {
	_, _ = s.SaveGroupInfo(groupID, name, "group_info", "", "{}")
}

// GroupName 读群名：先 KV，miss 再回源。
func (s *Scope) GroupName(groupID string) string {
	if s == nil || s.store == nil || groupID == "" {
		return ""
	}
	if value, ok, _ := s.store.kv.Get(nil, groupKVKey(s.workspaceID, groupID)); ok {
		return string(value)
	}
	var name string
	if err := s.store.db.Raw(`SELECT name FROM groups WHERE workspace_id = ? AND group_id = ?`,
		s.workspaceID, groupID).Scan(&name).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		s.store.noteFailure("读取群名", err)
	}
	if name != "" {
		_ = s.store.kv.Set(nil, groupKVKey(s.workspaceID, groupID), []byte(name), groupCacheTTL)
	}
	return name
}

// GroupType 读会话类型（groups.type）：group = 群聊，private = 私聊。
// 消息详情页要靠它在顶栏标出会话类型；直接查库不走 KV，
// 那个页面一次只问一个会话，没必要为它多养一份缓存。
func (s *Scope) GroupType(groupID string) string {
	if s == nil || s.store == nil || s.store.closed.Load() || groupID == "" {
		return ""
	}
	var kind string
	if err := s.store.db.Raw(`SELECT type FROM groups WHERE workspace_id = ? AND group_id = ?`,
		s.workspaceID, groupID).Scan(&kind).Error; err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		s.store.noteFailure("读取会话类型", err)
	}
	return kind
}

// LoadGroups 返回 group_id → 群名（没有名字的群值为空串）。
func (s *Scope) LoadGroups() (map[string]string, error) {
	if s == nil || s.store == nil {
		return nil, nil
	}
	var rows []groupRow
	if err := s.store.db.Where("workspace_id = ?", s.workspaceID).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.GroupID] = row.Name
	}
	return out, nil
}
