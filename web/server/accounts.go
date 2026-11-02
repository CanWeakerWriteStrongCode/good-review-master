package server

import (
	"sort"

	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/state"
	"good-review-master/workspace"
)

// AccountDetail 面板上的机器人账号概览。
// SelfID 是工作空间的接收账号（QQ 官方是 AppID）；群内 openid 按群变化，
// 不适合当账号标识。
type AccountDetail struct {
	SelfID           string `json:"self_id"`
	Nickname         string `json:"nickname"`
	GroupCount       int    `json:"group_count"`
	ActiveGroupCount int    `json:"active_group_count"` // 库里有消息记录的群数
	MessageCount     int    `json:"message_count"`
	LastActivity     string `json:"last_activity"`

	lastTime int64
}

// WorkspaceInfo 面板上的工作空间概览。
//
// 字段名沿用 platform_id / self_ids 是为了不重建内嵌前端：
// 新模型下 platform_id 是平台名，self_ids 只有接收账号一项。
type WorkspaceInfo struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	PlatformID       string   `json:"platform_id"`
	SelfIDs          []string `json:"self_ids"`
	GroupCount       int      `json:"group_count"`
	ActiveGroupCount int      `json:"active_group_count"`
	MessageCount     int      `json:"message_count"`
	LastActivity     string   `json:"last_activity"`
	Persistent       bool     `json:"persistent"` // state.db 是否可用；false 表示纯内存运行
}

// accountName 返回账号展示名（工作空间昵称）；找不到返回空串。
func (s *Server) accountName(account string) string {
	if s.workspaces == nil || account == "" {
		return ""
	}
	for _, view := range s.workspaces.Views() {
		if view.Account == account {
			return view.Name
		}
	}
	return ""
}

// collectGroupDetails 没有工作空间时的降级路径：库句柄都拿不到，
// 只能按配置列出群号，统计一律为空。
func (s *Server) collectGroupDetails(cfg *config.Config) ([]GroupDetail, []AccountDetail) {
	groupIDs := make([]string, 0, len(cfg.AllowGroups))
	for _, id := range cfg.AllowGroups {
		if id != "*" {
			groupIDs = append(groupIDs, id)
		}
	}
	return s.collectDetails(groupIDs, "", "", "", nil)
}

// collectWorkspaceDetails 构建某个工作空间的群列表与账号汇总。
//
// 群列表以库里有的群为准。bot.allow_groups 是所有账号共用的一份白名单，
// 里面混着别的账号看到的群；直接拿来当群列表，本账号库里没记录的那些群就会
// 在页面上变成「未归属账号」。group_openid 是按机器人分配的，同一个物理群
// 在不同账号眼里是两个ID，所以只能靠“谁库里有记录”来判定它属于哪个账号。
//
// 没开 state.db（纯内存运行）时没有库可查，才退回配置列表，数字全 0。
func (s *Server) collectWorkspaceDetails(view workspace.View) ([]GroupDetail, []AccountDetail) {
	stats := s.groupOverviews(view)
	groupIDs := make([]string, 0, len(stats))
	for groupID := range stats {
		groupIDs = append(groupIDs, groupID)
	}
	if view.State == nil {
		groupIDs = groupIDs[:0]
		for _, id := range view.Groups {
			if id != "*" {
				groupIDs = append(groupIDs, id)
			}
		}
	}
	// 按最后一条消息的时间倒序：刚聊过的群排最前，一条消息都没收到的（LastTime=0）沉底。
	// LastTime 就是汇总查询里的 MAX(ts)，已经在 stats 里了，不用再查一次。
	// 时间相同按群号排，保证同一个库每次打开顺序都一样。
	sort.Slice(groupIDs, func(i, j int) bool {
		li, lj := stats[groupIDs[i]].LastTime, stats[groupIDs[j]].LastTime
		if li != lj {
			return li > lj
		}
		return groupIDs[i] < groupIDs[j]
	})
	return s.collectDetails(groupIDs, view.Account, view.ID, view.Name, stats)
}

// groupOverviews 读这个工作空间在库里的群汇总。
//
// 库不可用（纯内存运行）或查询失败时返回空表：宁可在页面上不显示数字，
// 也不拿内存缓存冒充——那会让"消息数"变成"最近 500 条"，读数全靠猜。
func (s *Server) groupOverviews(view workspace.View) map[string]state.GroupOverview {
	if view.State == nil {
		return nil
	}
	list, err := view.State.GroupOverviews()
	if err != nil {
		logutil.Warn("读取群汇总失败，面板按无统计显示", "workspace", view.ID, "err", err)
		return nil
	}
	out := make(map[string]state.GroupOverview, len(list))
	for _, overview := range list {
		out[overview.GroupID] = overview
	}
	return out
}

// collectDetails 把群号列表与库里的统计拼成表格行。
//
// stats 里没有的群（配置里写了但一条消息都没收到）照样列出来，
// 只是数字全 0、HasMessages 为 false，页面显示「等待接收消息」。
func (s *Server) collectDetails(groupIDs []string, account, wsID, wsName string,
	stats map[string]state.GroupOverview) ([]GroupDetail, []AccountDetail) {

	var detail *AccountDetail
	ensure := func() *AccountDetail {
		if detail == nil {
			detail = &AccountDetail{SelfID: account, Nickname: wsName}
		}
		return detail
	}
	if account != "" {
		ensure()
	}

	seenGroup := make(map[string]struct{}, len(groupIDs))
	groups := make([]GroupDetail, 0, len(groupIDs))
	for _, groupID := range groupIDs {
		if groupID == "*" {
			continue
		}
		if _, dup := seenGroup[groupID]; dup {
			continue
		}
		seenGroup[groupID] = struct{}{}

		overview := stats[groupID]
		info := GroupDetail{
			GroupID:       groupID,
			GroupName:     overview.Name,
			Type:          overview.Type,
			WorkspaceID:   wsID,
			WorkspaceName: wsName,
			SelfID:        overview.SelfID,
			MessageCount:  overview.MessageCount,
			MemberCount:   overview.MemberCount,
			HasMessages:   overview.MessageCount > 0,
			lastTime:      overview.LastTime,
		}
		if overview.LastTime > 0 {
			info.LastActivity = formatTimestamp(overview.LastTime)
		}
		if info.HasMessages {
			info.AccountName = wsName
			if account != "" {
				acc := ensure()
				acc.GroupCount++
				acc.ActiveGroupCount++
				acc.MessageCount += overview.MessageCount
				if overview.LastTime > acc.lastTime {
					acc.lastTime = overview.LastTime
				}
			}
		}
		groups = append(groups, info)
	}

	if account == "" {
		return groups, nil
	}
	// 群数是全部（含还没收到消息的），ActiveGroupCount 才是有数据的那些。
	ensure().GroupCount = len(groups)
	list := make([]AccountDetail, 0, 1)
	if detail != nil {
		if detail.lastTime > 0 {
			detail.LastActivity = formatTimestamp(detail.lastTime)
		}
		list = append(list, *detail)
	}
	return groups, list
}

// workspaceInfo 汇总一个工作空间的展示数据。
func (s *Server) workspaceInfo(view workspace.View) WorkspaceInfo {
	groups, _ := s.collectWorkspaceDetails(view)
	info := WorkspaceInfo{
		ID:         view.ID,
		Name:       view.Name,
		PlatformID: view.Platform,
		GroupCount: len(groups),
		Persistent: view.HasState,
	}
	if view.Account != "" {
		info.SelfIDs = []string{view.Account}
	}
	var lastTime int64
	for _, g := range groups {
		if g.HasMessages {
			info.ActiveGroupCount++
		}
		info.MessageCount += g.MessageCount
		if g.lastTime > lastTime {
			lastTime = g.lastTime
		}
	}
	if lastTime > 0 {
		info.LastActivity = formatTimestamp(lastTime)
	}
	return info
}
