package state

// 面板群列表的汇总。和 conversation 里的缓存统计不是一回事：
// 缓存只留最近 max_cache_msg 条，重启回填也只回填那么多；
// 这里的数字是库里的全量记录——这个会话一共来过多少条消息、最后一次是什么时候。

// GroupOverview 是一个会话（群或私聊）在库里的汇总。
type GroupOverview struct {
	GroupID      string
	Name         string // 群名，平台没给过就是空串
	Type         string // group = 群聊，private = 私聊
	MessageCount int    // 库里该会话的消息总条数
	MemberCount  int    // members 表里收录的人数（说过话或被 @ 过）
	LastTime     int64  // 该会话最后一条消息的时间戳（秒）
	// SelfID 本账号的机器人 openid（存在 workspace_profile，v15 起不再逐群存）。
	// 同一个账号下每个会话拿到的都是同一个值；空串表示还没学到。
	SelfID string
}

// GroupOverviews 汇总本工作空间下每个会话的数据。
//
// 只读 groups 一张小表：计数列在写入时就维护好了（messages / members
// 的 AFTER INSERT 触发器直接累加），不再每次打开面板扫 messages 全表 GROUP BY。
// 机器人身份另取一次 workspace_profile：它是账号级的，所有会话共用一个值。
// 私聊也是 groups 里的一行（type=private），和群一样参与汇总。
func (s *Scope) GroupOverviews() ([]GroupOverview, error) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return nil, nil
	}

	var rows []groupOverviewRow
	if err := s.store.db.Raw(`
		SELECT group_id, name, type, message_count, member_count, last_ts
		FROM groups
		WHERE workspace_id = ?
		ORDER BY group_id`, s.workspaceID).Scan(&rows).Error; err != nil {
		s.store.noteFailure("汇总会话", err)
		return nil, err
	}
	// 读失败只意味着 SelfID 空着（面板顶栏退回账号名），不该让整张列表失败。
	selfUserID, _ := s.LoadSelfUserID()

	out := make([]GroupOverview, 0, len(rows))
	for _, row := range rows {
		out = append(out, GroupOverview{
			GroupID:      row.GroupID,
			Name:         row.Name,
			Type:         row.Type,
			MessageCount: row.MessageCount,
			MemberCount:  row.MemberCount,
			LastTime:     row.LastTS,
			SelfID:       selfUserID,
		})
	}
	return out, nil
}

// AllGroupOverviews 一条 SQL 读回多个工作空间的会话汇总，按 workspace_id 分组。
//
// 面板首屏要同时显示每个账号的会话列表，逐个账号调 GroupOverviews 就是 N 条
// 只差一个 WHERE 值的 SQL。这里用 IN 一次查完再在内存里分组，账号再多也只有一次往返。
// 机器人身份同样一次查回（workspace_profile），不按群各查一遍。
// 请求里没有的账号不会出现在返回的 map 里，调用方按 nil 处理即可。
func (s *Store) AllGroupOverviews(workspaceIDs []string) (map[string][]GroupOverview, error) {
	if s == nil || s.closed.Load() || len(workspaceIDs) == 0 {
		return nil, nil
	}

	var rows []groupOverviewRow
	if err := s.db.Raw(`
		SELECT workspace_id, group_id, name, type, message_count, member_count, last_ts
		FROM groups
		WHERE workspace_id IN ?
		ORDER BY workspace_id, group_id`, workspaceIDs).Scan(&rows).Error; err != nil {
		s.noteFailure("汇总多个工作空间的会话", err)
		return nil, err
	}
	selfIDs, err := s.selfUserIDs(workspaceIDs)
	if err != nil {
		s.noteFailure("读取机器人身份", err)
	}

	out := make(map[string][]GroupOverview, len(workspaceIDs))
	for _, row := range rows {
		out[row.WorkspaceID] = append(out[row.WorkspaceID], GroupOverview{
			GroupID:      row.GroupID,
			Name:         row.Name,
			Type:         row.Type,
			MessageCount: row.MessageCount,
			MemberCount:  row.MemberCount,
			LastTime:     row.LastTS,
			SelfID:       selfIDs[row.WorkspaceID],
		})
	}
	return out, nil
}

// selfUserIDs 批量读各工作空间的机器人 openid，一次查完。
func (s *Store) selfUserIDs(workspaceIDs []string) (map[string]string, error) {
	var rows []struct {
		WorkspaceID string `gorm:"column:workspace_id"`
		SelfUserID  string `gorm:"column:self_user_id"`
	}
	if err := s.db.Raw(`SELECT workspace_id, self_user_id FROM workspace_profile
		WHERE workspace_id IN ?`, workspaceIDs).Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.WorkspaceID] = row.SelfUserID
	}
	return out, nil
}

// groupOverviewRow 是汇总查询的扫描目标。
// WorkspaceID 只在批量查询里 SELECT，单账号查询时留空。
type groupOverviewRow struct {
	WorkspaceID  string `gorm:"column:workspace_id"`
	GroupID      string `gorm:"column:group_id"`
	Name         string `gorm:"column:name"`
	Type         string `gorm:"column:type"`
	MessageCount int    `gorm:"column:message_count"`
	MemberCount  int    `gorm:"column:member_count"`
	LastTS       int64  `gorm:"column:last_ts"`
}

// ResetData 删掉本工作空间在库里的一切业务数据（仅调试与测试用）。
//
// 面板现在只从库读数，清内存已经不够了：不清库，重置后的页面还是上一次注入的那些群。
func (s *Scope) ResetData() error {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return nil
	}
	// 群名的 KV 缓存按 (workspace, group) 存，删行前先把 key 收齐。
	names, err := s.LoadGroups()
	if err != nil {
		return err
	}
	// groups 一张表带着群名、计数和人格（v11 / v13 先后并进来），不用单独清。
	for _, table := range []string{"messages", "anchors", "members", "groups"} {
		if err := s.store.db.Exec(`DELETE FROM `+table+` WHERE workspace_id = ?`, s.workspaceID).Error; err != nil {
			s.store.noteFailure("清空 "+table, err)
			return err
		}
	}
	// 机器人身份 v15 起在 workspace_profile 里，不在 groups 行上：不单独清的话，
	// 重置后它还在，历史消息仍会被当成机器人自己说的。
	if err := s.store.db.Exec(`UPDATE workspace_profile SET self_user_id = ''
		WHERE workspace_id = ?`, s.workspaceID).Error; err != nil {
		s.store.noteFailure("清空机器人身份", err)
		return err
	}
	for groupID := range names {
		_ = s.store.kv.Delete(nil, groupKVKey(s.workspaceID, groupID))
	}
	return nil
}
