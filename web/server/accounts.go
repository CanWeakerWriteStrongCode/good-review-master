package server

import (
	"sort"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/onebot"
	"good-review-master/workspace"
)

// AccountDetail 面板上的机器人账号概览。
// SelfID 是 OneBot 的 self_id：QQ 官方是 openid，aiocqhttp 是数字 QQ。
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
type WorkspaceInfo struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	PlatformID       string   `json:"platform_id"`
	SelfIDs          []string `json:"self_ids"`
	GroupCount       int      `json:"group_count"`
	CachedGroupCount int      `json:"cached_group_count"`
	MessageCount     int      `json:"message_count"`
	LastActivity     string   `json:"last_activity"`
}

// accountName 返回账号昵称；解析不到时返回空串，由前端回落到 self_id。
// 结果按 self_id 缓存：面板每次刷新都会问，但昵称几乎不会变。
func (s *Server) accountName(selfID string) string {
	if onebot.IsSelfIDPlaceholder(selfID) {
		return ""
	}
	s.accountNamesMu.RLock()
	name := s.accountNames[selfID]
	s.accountNamesMu.RUnlock()
	if name != "" || s.obClient == nil {
		return name
	}
	info, err := s.obClient.GetLoginInfoFor(selfID)
	// 插件不知道这个账号时会退回"第一个适配器"，Matches 用来拦住这种错误昵称。
	if err != nil || info == nil || info.Nickname == "" || !info.Matches(selfID) {
		return ""
	}
	s.accountNamesMu.Lock()
	s.accountNames[selfID] = info.Nickname
	s.accountNamesMu.Unlock()
	return info.Nickname
}

// accountIDs 返回面板已知的机器人账号（旧接口/测试路径，基于进程级默认缓存）。
// allow_self_ids 显式列出时以它为准；通配符 "*" 无法枚举，只能取缓存里出现过的接收账号。
func accountIDs(cfg *config.Config) []string {
	return accountIDsFor(cfg.AllowSelfIDs, cache.DefaultStore(), panelGroupIDs(cfg))
}

func accountIDsFor(declared []string, store *cache.Store, groupIDs []string) []string {
	seen := make(map[string]struct{})
	for _, id := range declared {
		if id != "" && id != "*" {
			seen[id] = struct{}{}
		}
	}
	for _, groupID := range groupIDs {
		gc := store.GetCache(groupID)
		if gc == nil {
			continue
		}
		if selfID := gc.Stats().SelfID; selfID != "" {
			seen[selfID] = struct{}{}
		}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// collectGroupDetails 旧接口路径：全局白名单 + 进程级默认缓存。
func (s *Server) collectGroupDetails(cfg *config.Config) ([]GroupDetail, []AccountDetail) {
	return s.collectDetails(panelGroupIDs(cfg), cfg.AllowSelfIDs, cache.DefaultStore(), "", "")
}

// collectWorkspaceDetails 构建某个工作空间的群列表与账号汇总。
func (s *Server) collectWorkspaceDetails(view workspace.View) ([]GroupDetail, []AccountDetail) {
	groupIDs := view.Groups
	if len(groupIDs) == 0 || containsWildcard(groupIDs) {
		groupIDs = view.Store.ListGroupIDs()
		sort.Strings(groupIDs)
	}
	groups, accounts := s.collectDetails(groupIDs, view.SelfIDs, view.Store, view.ID, view.Name)
	return groups, accounts
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
// 账号归属来自群缓存最近一条消息的 self_id：QQ 官方的 group_openid 按 bot 维度
// 分配，同一个群在不同账号下就是不同的 group_id；通用 OneBot 下则由工作空间
// 本身把同名群分开，所以这里只需在单个 Store 内归属。
func (s *Server) collectDetails(groupIDs, declaredSelfIDs []string, store *cache.Store, wsID, wsName string) ([]GroupDetail, []AccountDetail) {
	store = storeOr(store)
	cachedIDs := store.ListGroupIDs()
	cachedSet := make(map[string]struct{}, len(cachedIDs))
	for _, id := range cachedIDs {
		cachedSet[id] = struct{}{}
	}

	accounts := make(map[string]*AccountDetail)
	ensure := func(selfID string) *AccountDetail {
		acc := accounts[selfID]
		if acc == nil {
			acc = &AccountDetail{SelfID: selfID, Nickname: s.accountName(selfID)}
			accounts[selfID] = acc
		}
		return acc
	}
	for _, id := range declaredSelfIDs {
		if id != "" && id != "*" {
			ensure(id)
		}
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
		// 获取群名称（优先用缓存，否则调 AstrBot 桥接 API）
		s.groupNamesMu.RLock()
		name, ok := s.groupNames[groupID]
		s.groupNamesMu.RUnlock()
		if !ok && s.obClient != nil {
			gi, err := s.obClient.GetGroupInfo(groupID)
			if err == nil && gi.GroupName != "" {
				name = gi.GroupName
				s.groupNamesMu.Lock()
				s.groupNames[groupID] = name
				s.groupNamesMu.Unlock()
			}
		}
		info.GroupName = name

		if _, ok := cachedSet[groupID]; ok {
			if gc := store.GetCache(groupID); gc != nil {
				stats := gc.Stats()
				info.Cached = true
				info.MessageCount = stats.Count
				info.SelfID = stats.SelfID
				info.AccountName = s.accountName(stats.SelfID)
				if stats.LastTime > 0 {
					info.LastActivity = formatTimestamp(stats.LastTime)
				}
				if stats.SelfID != "" {
					acc := ensure(stats.SelfID)
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

	list := make([]AccountDetail, 0, len(accounts))
	for _, acc := range accounts {
		if acc.lastTime > 0 {
			acc.LastActivity = formatTimestamp(acc.lastTime)
		}
		list = append(list, *acc)
	}
	// 有昵称的排前面；同组按昵称/self_id 排，保证刷新时顺序稳定。
	sort.SliceStable(list, func(i, j int) bool {
		if (list[i].Nickname == "") != (list[j].Nickname == "") {
			return list[i].Nickname != ""
		}
		ni, nj := list[i].Nickname, list[j].Nickname
		if ni == "" {
			ni = list[i].SelfID
		}
		if nj == "" {
			nj = list[j].SelfID
		}
		return ni < nj
	})
	return groups, list
}

// workspaceInfo 汇总一个工作空间的展示数据。
func (s *Server) workspaceInfo(view workspace.View) WorkspaceInfo {
	groups, _ := s.collectWorkspaceDetails(view)
	info := WorkspaceInfo{
		ID:         view.ID,
		Name:       view.Name,
		PlatformID: view.PlatformID,
		SelfIDs:    view.SelfIDs,
		GroupCount: len(groups),
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
