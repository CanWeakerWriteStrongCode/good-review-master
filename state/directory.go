package state

// members / groups / member_info_history / group_info_history：
// 这个群叫什么、群里谁叫什么、这个账号的机器人是哪个 openid
// （workspace_profile.self_user_id，v15 起从 groups 提上来——它对所有群都一样）。
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
	// ID 是自增主键；三元组是业务键（表上是 UNIQUE），upsert 的冲突目标按它写。
	ID          int64  `gorm:"column:id;primaryKey;autoIncrement"`
	WorkspaceID string `gorm:"column:workspace_id"`
	GroupID     string `gorm:"column:group_id"`
	UserID      string `gorm:"column:user_id"`
	Name        string `gorm:"column:name"`
	DisplayName string `gorm:"column:display_name"`
	Extra       string `gorm:"column:extra"`
	UpdatedAt   int64  `gorm:"column:updated_at"`
}

func (memberRow) TableName() string { return "members" }

type groupRow struct {
	// ID 是自增主键；(workspace_id, group_id) 是业务键（表上是 UNIQUE）。
	ID          int64  `gorm:"column:id;primaryKey;autoIncrement"`
	WorkspaceID string `gorm:"column:workspace_id"`
	GroupID     string `gorm:"column:group_id"`
	Name        string `gorm:"column:name"`
	// Type 会话类型：group = 群聊，private = 私聊。
	// 私聊在库里的 group_id 是 private:<openid>，和群号一样按会话存。
	Type string `gorm:"column:type"`
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

// SaveSelfUserID 记住这个账号的机器人 openid。
//
// 存在 workspace_profile 里，不逐群存：openid 是账号维度的事实——同一个机器人在
// 所有群的 openid 相同（实测同账号 3 个群一个值），学到一个群就等于全账号都有，
// 不必等每个群各 @ 一次。调用方负责别把哨兵值写进来（见 workspace.LearnIdentity）。
func (s *Scope) SaveSelfUserID(selfUserID string) {
	if s == nil || s.store == nil || selfUserID == "" || s.store.closed.Load() {
		return
	}
	// UPSERT：画像行一般早就在了，但调试注入或纯内存跑起来时可能还没有。
	// 值没变就不更新（WHERE 只放行真变化的那种），免得每条消息都写一遍。
	if err := s.store.db.Exec(`INSERT INTO workspace_profile
			(workspace_id, self_user_id, extra, created_at, updated_at)
		VALUES (?, ?, '{}', ?, ?)
		ON CONFLICT(workspace_id) DO UPDATE SET self_user_id = excluded.self_user_id
		WHERE workspace_profile.self_user_id <> excluded.self_user_id`,
		s.workspaceID, selfUserID, time.Now().Unix(), time.Now().Unix()).Error; err != nil {
		s.store.noteFailure("保存机器人身份", err)
	}
}

// LoadSelfUserID 读学到的机器人 openid；没学到时返回空串，不是错误。
func (s *Scope) LoadSelfUserID() (string, error) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return "", nil
	}
	var selfUserID string
	err := s.store.db.Raw(`SELECT self_user_id FROM workspace_profile WHERE workspace_id = ?`,
		s.workspaceID).Scan(&selfUserID).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		s.store.noteFailure("读取机器人身份", err)
		return "", err
	}
	return selfUserID, nil
}

// SelfUserID 同 LoadSelfUserID，但失败只记日志、返回空串：面板调用不该因此报错。
//
// 面板拿它认出「哪些消息是机器人自己说的」：那一行的发送者名字不能只靠 members，
// 那里存的是群名片、也可能根本没有这一行。
func (s *Scope) SelfUserID() string {
	selfUserID, _ := s.LoadSelfUserID()
	return selfUserID
}

// BackfillSelfUserID 把这个账号下机器人自己发的消息补上发送者 ID，返回改了多少行。
//
// 出站回复在没有 self.user_id 时只能存空串（见 onebot.Event.BotID），
// 身份学到的那一刻再回来补。openid 是全账号通用的，所以一次能把所有群的都补掉。
//
// WHERE 两个条件都不能少：
//   - user_id 为空：已经有发送者的消息绝不覆盖，那是别人的话；
//   - message_id 以 bot- 开头：出站回复的 ID 都带这个前缀（见 speech.recordSelfReply），
//     入站消息的 ID 来自平台，这样不会把「入站时没给 user_id 的怪消息」也认成机器人。
func (s *Scope) BackfillSelfUserID(selfUserID string) (int64, error) {
	if s == nil || s.store == nil || selfUserID == "" || s.store.closed.Load() {
		return 0, nil
	}
	result := s.store.db.Model(&messageRow{}).
		Where("workspace_id = ? AND user_id = '' AND message_id LIKE 'bot-%'", s.workspaceID).
		Update("user_id", selfUserID)
	if result.Error != nil {
		s.store.noteFailure("回填机器人发送者ID", result.Error)
		return 0, result.Error
	}
	return result.RowsAffected, nil
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

// MemberCount 读这个会话已收录的成员数（groups.member_count，插入成员时由触发器累加）。
//
// 和面板群列表同一个口径：说过话或被 @ 过的人，不是群的真实人数——QQ 官方没有
// 成员列表接口，只能数观察到的。私聊也走这张表，那边一般只有对聊的一个人。
func (s *Scope) MemberCount(groupID string) int {
	if s == nil || s.store == nil || s.store.closed.Load() || groupID == "" {
		return 0
	}
	var count int
	if err := s.store.db.Raw(`SELECT member_count FROM groups WHERE workspace_id = ? AND group_id = ?`,
		s.workspaceID, groupID).Scan(&count).Error; err != nil {
		s.store.noteFailure("读取会话成员数", err)
	}
	return count
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
	// 行可能已经被消息/成员触发器建出来了（先有消息、后有群名），
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
