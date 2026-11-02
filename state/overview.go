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
	// SelfID 本会话学到的机器人 openid（存在 groups.self_user_id 列），空串表示还没学到。
	SelfID string
}

// GroupOverviews 汇总本工作空间下每个会话的数据。
//
// 只读 groups 一张小表：计数列在写入时就维护好了（messages / members
// 的 AFTER INSERT 触发器直接累加），不再每次打开面板扫 messages 全表 GROUP BY；
// 机器人身份也在同一行（自 v12 起 identities 已并进来），不用再 JOIN。
// 私聊也是 groups 里的一行（type=private），和群一样参与汇总。
func (s *Scope) GroupOverviews() ([]GroupOverview, error) {
	if s == nil || s.store == nil || s.store.closed.Load() {
		return nil, nil
	}

	var rows []groupOverviewRow
	if err := s.store.db.Raw(`
		SELECT group_id, name, type, self_user_id, message_count, member_count, last_ts
		FROM groups
		WHERE workspace_id = ?
		ORDER BY group_id`, s.workspaceID).Scan(&rows).Error; err != nil {
		s.store.noteFailure("汇总会话", err)
		return nil, err
	}

	out := make([]GroupOverview, 0, len(rows))
	for _, row := range rows {
		out = append(out, GroupOverview{
			GroupID:      row.GroupID,
			Name:         row.Name,
			Type:         row.Type,
			MessageCount: row.MessageCount,
			MemberCount:  row.MemberCount,
			LastTime:     row.LastTS,
			SelfID:       row.SelfUserID,
		})
	}
	return out, nil
}

// groupOverviewRow 是汇总查询的扫描目标。
type groupOverviewRow struct {
	GroupID      string `gorm:"column:group_id"`
	Name         string `gorm:"column:name"`
	Type         string `gorm:"column:type"`
	SelfUserID   string `gorm:"column:self_user_id"`
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
	for _, table := range []string{"messages", "anchors", "members", "groups", "group_personas"} {
		if err := s.store.db.Exec(`DELETE FROM `+table+` WHERE workspace_id = ?`, s.workspaceID).Error; err != nil {
			s.store.noteFailure("清空 "+table, err)
			return err
		}
	}
	for groupID := range names {
		_ = s.store.kv.Delete(nil, groupKVKey(s.workspaceID, groupID))
	}
	return nil
}
