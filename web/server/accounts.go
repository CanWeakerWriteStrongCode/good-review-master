package server

import (
	"sort"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/workspace"
)

// AccountDetail 面板上的机器人账号概览。
// SelfID 是工作空间的接收账号（QQ 官方是 AppID）；群内 openid 按群变化，
// 不适合当账号标识。
type AccountDetail struct {
	SelfID           string `json:"self_id"`
	Nickname         string `json:"nickname"`
	GroupCount       int    `json:"group_count"`
	CachedGroupCount int    `json:"cached_group_count"`
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
	CachedGroupCount int      `json:"cached_group_count"`
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

// collectGroupDetails 旧接口路径：全局白名单 + 进程级默认缓存。
func (s *Server) collectGroupDetails(cfg *config.Config) ([]GroupDetail, []AccountDetail) {
	return s.collectDetails(panelGroupIDs(cfg), "", cache.DefaultStore(), "", "")
}

// collectWorkspaceDetails 构建某个工作空间的群列表与账号汇总。
func (s *Server) collectWorkspaceDetails(view workspace.View) ([]GroupDetail, []AccountDetail) {
	groupIDs := view.Groups
	if len(groupIDs) == 0 || containsWildcard(groupIDs) {
		groupIDs = view.Store.ListGroupIDs()
		sort.Strings(groupIDs)
	}
	return s.collectDetails(groupIDs, view.Account, view.Store, view.ID, view.Name)
}

func containsWildcard(groups []string) bool {
	for _, g := range groups {
		if g == "*" {
			return true
		}
	}
	return false
}

// collectDetails 从指定 Store 构建群列表与账号汇总。
//
// 一个工作空间只对应一个接收账号，所以汇总里最多一条；群里的 self_id 是
// 事件携带的群内 openid（QQ 官方按群分配），只作展示，不再当账号键。
func (s *Server) collectDetails(groupIDs []string, account string, store *cache.Store,
	wsID, wsName string) ([]GroupDetail, []AccountDetail) {
	store = storeOr(store)
	cachedIDs := store.ListGroupIDs()
	cachedSet := make(map[string]struct{}, len(cachedIDs))
	for _, id := range cachedIDs {
		cachedSet[id] = struct{}{}
	}

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

		info := GroupDetail{GroupID: groupID, WorkspaceID: wsID, WorkspaceName: wsName}
		// 群名优先用 state.db 里记录过的值，再退到进程缓存和实现端查询。
		if s.workspaces != nil {
			info.GroupName = s.workspaces.GroupName(wsID, groupID)
			info.MemberCount = s.workspaces.MemberCount(wsID, groupID)
		}
		s.groupNamesMu.RLock()
		name, ok := s.groupNames[groupID]
		s.groupNamesMu.RUnlock()
		if info.GroupName == "" {
			info.GroupName = name
		}
		if !ok && info.GroupName == "" && s.obClient != nil {
			gi, err := s.obClient.For(wsID).GetGroupInfo(groupID)
			if err == nil && gi.GroupName != "" {
				info.GroupName = gi.GroupName
				s.groupNamesMu.Lock()
				s.groupNames[groupID] = gi.GroupName
				s.groupNamesMu.Unlock()
			}
		}

		if _, ok := cachedSet[groupID]; ok {
			if gc := store.GetCache(groupID); gc != nil {
				stats := gc.Stats()
				info.Cached = true
				info.MessageCount = stats.Count
				info.SelfID = stats.SelfID
				info.AccountName = wsName
				if stats.LastTime > 0 {
					info.LastActivity = formatTimestamp(stats.LastTime)
				}
				if account != "" {
					acc := ensure()
					acc.GroupCount++
					acc.CachedGroupCount++
					acc.MessageCount += stats.Count
					if stats.LastTime > acc.lastTime {
						acc.lastTime = stats.LastTime
					}
				}
			}
		}
		groups = append(groups, info)
	}

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
		if g.Cached {
			info.CachedGroupCount++
		}
		info.MessageCount += g.MessageCount
	}
	for _, groupID := range view.Store.ListGroupIDs() {
		if gc := view.Store.GetCache(groupID); gc != nil {
			if t := gc.Stats().LastTime; t > lastTime {
				lastTime = t
			}
		}
	}
	if lastTime > 0 {
		info.LastActivity = formatTimestamp(lastTime)
	}
	return info
}

func storeOr(store *cache.Store) *cache.Store {
	if store == nil {
		return cache.DefaultStore()
	}
	return store
}
