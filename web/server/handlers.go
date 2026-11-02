package server

import (
	"crypto/subtle"
	"net/http"
	"sort"

	"good-review-master/config"
	"good-review-master/conversation"
	"good-review-master/logutil"
	"good-review-master/policy"
	"good-review-master/workspace"

	"github.com/gin-gonic/gin"
)

// APIResponse 统一 API 响应信封。
//
// 成功时只有 code=200 + data；失败时 code 是六位业务码、err 是它的名字、
// msg 是按请求语言翻好的一句话。**前端只认 code != 200，然后弹 msg**，
// 不需要认识任何一个具体业务码——所以后端加错误码不用前端跟着发版。
type APIResponse struct {
	Code int         `json:"code"`
	Data interface{} `json:"data"`
	// Err 是错误码的名字（如 workspace.not_found），给日志和浏览器网络面板看。
	// 前端不依赖它，但排查时 "60001" 远不如这个名字直观。
	Err string `json:"err,omitempty"`
	// Msg 已按请求语言翻译好，前端拿到直接弹。
	Msg string `json:"msg,omitempty"`
	// ReqID 只在兜底的未知错误里出现，是给用户报障用的单号。
	ReqID string `json:"req_id,omitempty"`
}

// GroupDetail 群信息
type GroupDetail struct {
	GroupID   string `json:"group_id"`
	GroupName string `json:"group_name"`
	// Type 会话类型：group = 群聊，private = 私聊。
	Type          string `json:"type"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	SelfID        string `json:"self_id"`      // 接收该群的机器人账号；空=无法归属
	AccountName   string `json:"account_name"` // 账号昵称；解析不到时为空
	MessageCount  int    `json:"message_count"`
	MemberCount   int    `json:"member_count"` // 已收录成员数（说过话或被 @ 过）
	// LastTS 是最近一条消息的时间（Unix 秒），0 表示没有记录。
	// 接口只传原始时间戳，不在后端拼日期字符串：日期格式跟习惯走更合适，
	// 而后端的 i18n 只负责错误文案，不必把日期格式化规则也搬过来。
	LastTS int64 `json:"last_ts"`
	// HasMessages 表示 state.db 里这个群有消息记录。名字不叫 cached：
	// 面板已经不看内存缓存了，这个值来自 messages 表的条数。
	HasMessages bool `json:"has_messages"`
}

// BotStatus 面板顶部的运行时状态。
// 不包含 API Key：以后可能同时挂多个模型/接口，顶部只写账号数和群数。
type BotStatus struct {
	BotNickname    string `json:"bot_nickname"`
	GroupCount     int    `json:"group_count"`
	AccountCount   int    `json:"account_count"`
	AllowAllGroups bool   `json:"allow_all_groups"`
}

// panelGroupIDs 返回管理面板应展示的群列表。
// 配置了通配符 "*" 时没有固定群号可列，只能展示已经产生缓存消息的群。
func panelGroupIDs(cfg *config.Config) []string {
	if !policy.AllowsAllStreams(cfg) {
		return cfg.AllowGroups
	}
	ids := conversation.ListGroupIDs()
	sort.Strings(ids)
	return ids
}

func (s *Server) handleAPIGroups() gin.HandlerFunc {
	return func(c *gin.Context) {
		// 一个请求内取一次快照：群列表与 bot 信息必须来自同一份配置，
		// 否则热更新插在中间会返回"群列表是新的、群数量是旧的"这种自相矛盾的响应
		cfg := s.cfg()
		var groups []GroupDetail
		var accounts []AccountDetail
		if views := s.workspaceViews(); len(views) > 0 {
			if len(views) != 1 {
				// 多个账号时没法猜用户要看谁的，只能让他明确指定
				fail(c, CodeMultipleWorkspaces, map[string]any{"Endpoint": "/api/workspaces/{wid}/groups"})
				return
			}
			groups, accounts = s.collectWorkspaceDetails(views[0])
		} else {
			groups, accounts = s.collectGroupDetails(cfg)
		}

		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: gin.H{
				"groups":   groups,
				"accounts": accounts,
				"bot_info": s.botStatusTotal(cfg),
			},
		})
	}
}

func (s *Server) workspaceViews() []workspace.View {
	if s.workspaces == nil {
		return nil
	}
	return s.workspaces.Views()
}

func (s *Server) botStatus(cfg *config.Config, groupCount, accountCount int) BotStatus {
	return BotStatus{
		BotNickname:    cfg.BotNickname,
		GroupCount:     groupCount,
		AccountCount:   accountCount,
		AllowAllGroups: policy.AllowsAllStreams(cfg),
	}
}

// botStatusTotal 是面板顶部状态栏的数字：所有账号加起来多少个群。
//
// 不能拿当前选中的那个工作空间的数字。切账号时会被重新拉取，
// 状态栏就从「2 个账号 | 5 个群」跳成「1 个账号 | 4 个群」，看起来像换了台机器。
func (s *Server) botStatusTotal(cfg *config.Config) BotStatus {
	return s.botStatusOf(cfg, s.workspaceViews())
}

// botStatusOf 按给定的工作空间算状态栏数字。
// 没有工作空间（旧版本或纯内存运行）时只能按配置里的群白名单数，账号数给 0。
func (s *Server) botStatusOf(cfg *config.Config, views []workspace.View) BotStatus {
	if len(views) == 0 {
		return s.botStatus(cfg, len(panelGroupIDs(cfg)), 0)
	}
	// 走批量：所有账号的会话汇总一条 SQL 读完。
	details := s.collectAllWorkspaceDetails(views)
	list := make([]WorkspaceInfo, 0, len(views))
	for _, view := range views {
		list = append(list, details[view.ID].Info)
	}
	return s.botStatusTotals(cfg, list)
}

// botStatusTotals 把各工作空间的汇总累加成状态栏数字。
// 所有接口的 bot_info 都走这里，避免同一个口径写在两处、以后改一处漏一处。
func (s *Server) botStatusTotals(cfg *config.Config, list []WorkspaceInfo) BotStatus {
	groupCount, accountCount := 0, 0
	for _, info := range list {
		groupCount += info.GroupCount
		accountCount += len(info.SelfIDs)
	}
	return s.botStatus(cfg, groupCount, accountCount)
}

// handleAPISessions 把所有账号下的会话平铺成一份列表，按最后一条消息的时间倒序。
//
// 面板的「全部会话」视图用它：不用先选账号就能看到所有群和私聊，
// 每条都带 workspace_id / workspace_name，卡片上写清楚归谁监控。
func (s *Server) handleAPISessions() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := s.cfg()
		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: gin.H{
				"sessions": s.collectSessions(s.workspaceViews()),
				"bot_info": s.botStatusTotal(cfg),
			},
		})
	}
}

// collectSessions 把各工作空间的会话拼成一张表，按最近消息时间倒序。
//
// 同一个物理群在不同机器人眼里是两个 group_openid，两边群名也拿不到，
// 没法可靠判重，所以不合并：一条会话就是一条（账号 + 群号）。
func (s *Server) collectSessions(views []workspace.View) []GroupDetail {
	// 一次查询把全部账号的会话读回来，不逐个账号查。
	details := s.collectAllWorkspaceDetails(views)
	all := make([]GroupDetail, 0, len(views)*4)
	for _, view := range views {
		all = append(all, details[view.ID].Groups...)
	}
	// 时间相同的按账号、再按群号，保证同一个库每次打开顺序一致。
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].LastTS != all[j].LastTS {
			return all[i].LastTS > all[j].LastTS
		}
		if all[i].WorkspaceID != all[j].WorkspaceID {
			return all[i].WorkspaceID < all[j].WorkspaceID
		}
		return all[i].GroupID < all[j].GroupID
	})
	return all
}

// handleAPIWorkspaces 返回工作空间列表与各自汇总。
//
// with_groups=1 时顺带返回每个账号的会话列表（groups_by_workspace）：
// 面板要同时显示账号汇总和每个账号下的群，分两次请求会把每个账号的 groups 表
// 读两遍。带上这个参数，一个账号只查一次库，前端也只发一个请求。
func (s *Server) handleAPIWorkspaces() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := s.cfg()
		views := s.workspaceViews()

		// 一次查库把全部账号的会话读回来，汇总和群列表都从这份结果里算。
		details := s.collectAllWorkspaceDetails(views)
		list := make([]WorkspaceInfo, 0, len(views))
		byWorkspace := make(map[string][]GroupDetail, len(views))
		for _, view := range views {
			d := details[view.ID]
			list = append(list, d.Info)
			byWorkspace[view.ID] = d.Groups
		}

		data := gin.H{
			"workspaces": list,
			"bot_info":   s.botStatusTotals(cfg, list),
		}
		if c.Query("with_groups") == "1" {
			data["groups_by_workspace"] = byWorkspace
		}
		c.JSON(http.StatusOK, APIResponse{Code: 200, Data: data})
	}
}

// handleAPIWorkspaceGroups 返回指定工作空间的群与账号汇总。
//
// 拉的是单个账号的群，但 bot_info 仍然是全局口径：状态栏写"几个账号、
// 一共几个群"，不能让切账号把它改成当前账号的数字。
func (s *Server) handleAPIWorkspaceGroups() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := s.cfg()
		views := s.workspaceViews()
		wid := c.Param("wid")

		var target workspace.View
		found := false
		for _, view := range views {
			if view.ID == wid {
				target, found = view, true
				break
			}
		}
		if !found {
			fail(c, CodeWorkspaceNotFound, nil)
			return
		}

		// 一次查库同时拿到这个账号的群和全局 bot_info 需要的各账号汇总。
		details := s.collectAllWorkspaceDetails(views)
		list := make([]WorkspaceInfo, 0, len(views))
		for _, view := range views {
			list = append(list, details[view.ID].Info)
		}
		d := details[target.ID]

		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: gin.H{
				"workspace": d.Info,
				"groups":    d.Groups,
				"accounts":  d.Accounts,
				"bot_info":  s.botStatusTotals(cfg, list),
			},
		})
	}
}

func (s *Server) workspaceView(id string) (workspace.View, bool) {
	if s.workspaces == nil {
		return workspace.View{}, false
	}
	return s.workspaces.ViewByID(id)
}

// handleAPIMessages 返回指定群的缓存消息。
// groupNames 由 handleAPIGroups 与 debug inject/reset 并发读写，读也要持锁——
// 之前这里直读 map，是已知的数据竞争点。
func (s *Server) handleAPIMessages() gin.HandlerFunc {
	return func(c *gin.Context) {
		var view workspace.View
		if views := s.workspaceViews(); len(views) > 0 {
			if len(views) != 1 {
				fail(c, CodeMultipleWorkspaces, map[string]any{"Endpoint": "/api/workspaces/{wid}/groups/{gid}"})
				return
			}
			view = views[0]
		}
		s.writeMessages(c, c.Param("id"), view)
	}
}

// handleAPIWorkspaceMessages 返回指定工作空间下某个群的消息。
func (s *Server) handleAPIWorkspaceMessages() gin.HandlerFunc {
	return func(c *gin.Context) {
		view, ok := s.workspaceView(c.Param("wid"))
		if !ok {
			fail(c, CodeWorkspaceNotFound, nil)
			return
		}
		s.writeMessages(c, c.Param("gid"), view)
	}
}

// panelMessageLimit 是消息详情页一次返回的条数上限。
// 群里的记录可能上万条，全量吐给前端会卡；页头显示库里总数，列表只给最近这么多条。
const panelMessageLimit = 500

// writeMessages 返回某个群的消息记录。
//
// 数据来自 state.db 的 messages 表（最近 panelMessageLimit 条，时间升序），
// 昵称从 members 表补。只有没开 state.db（纯内存运行）或查库失败时才退回内存缓存。
func (s *Server) writeMessages(c *gin.Context, groupID string, view workspace.View) {
	groupName := ""
	groupType := ""
	if view.State != nil {
		groupName = view.State.GroupName(groupID)
		// 详情页要靠它区分私聊和群聊（私聊的键是 private:<openid>），
		// 所以这里跟群名一起读出来，前端不用再猜 group_id 前缀。
		groupType = view.State.GroupType(groupID)
	}
	wsID, wsName := view.ID, view.Name

	if view.State != nil {
		msgs, total, err := view.State.Messages(groupID, panelMessageLimit)
		if err != nil {
			logutil.Warn("读取群消息失败，退回内存缓存", "workspace", wsID, "group", groupID, "err", err)
		} else {
			c.JSON(http.StatusOK, APIResponse{
				Code: 200,
				Data: gin.H{
					"group_id":       groupID,
					"group_name":     groupName,
					"type":           groupType,
					"workspace_id":   wsID,
					"workspace_name": wsName,
					"self_id":        view.Account,
					"account_name":   wsName,
					"messages":       msgs,
					"total":          total,
					"empty":          total == 0,
				},
			})
			return
		}
	}

	store := view.Store
	if store == nil {
		store = conversation.DefaultStore()
	}
	gc := store.GetCache(groupID)
	if gc == nil {
		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: gin.H{
				"group_id":       groupID,
				"group_name":     groupName,
				"type":           groupType,
				"workspace_id":   wsID,
				"workspace_name": wsName,
				"self_id":        view.Account,
				"account_name":   wsName,
				"messages":       []conversation.Message{},
				"total":          0,
				"empty":          true,
			},
		})
		return
	}

	stats := gc.Stats()
	c.JSON(http.StatusOK, APIResponse{
		Code: 200,
		Data: gin.H{
			"group_id":       groupID,
			"group_name":     groupName,
			"type":           groupType,
			"workspace_id":   wsID,
			"workspace_name": wsName,
			"self_id":        view.Account,
			"account_name":   wsName,
			"messages":       gc.GetAll(),
			"total":          stats.Count,
			"empty":          stats.Count == 0,
		},
	})
}

func (s *Server) handleAPIStatus() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: s.botStatusTotal(s.cfg()),
		})
	}
}

// handleLogin 登录校验，成功返回 token（web_password 必填，无免密通道）。
//
// 限流按来源 IP 施加，且**只对失败计数**（成功不消费令牌）：
// 正常用户不会被自己拖慢，暴力破解则在若干次失败后必须等桶回填。
func handleLogin(username, password, jwtSecret string, limiter *loginRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		clientIP := c.ClientIP()
		if limiter.Blocked(clientIP) {
			logutil.Warn("登录失败次数过多，已限流", "ip", clientIP)
			// Retry-After 是标准头，网关和浏览器都认，所以留在 HTTP 这一层
			c.Header("Retry-After", "60")
			fail(c, CodeRateLimited, nil)
			return
		}

		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			fail(c, CodeBadJSON, nil)
			return
		}
		// 常量时间比较：避免用响应时间逐字节猜密码（subtle 的用法见 chacha20 文档）
		if subtle.ConstantTimeCompare([]byte(body.Username), []byte(username)) != 1 ||
			subtle.ConstantTimeCompare([]byte(body.Password), []byte(password)) != 1 {
			limiter.RecordFailure(clientIP)
			logutil.Warn("登录失败", "ip", clientIP, "username", body.Username)
			// 业务判断（比过了、不相等），所以是 HTTP 200 + 业务码，不是 401
			fail(c, CodeBadCredentials, nil)
			return
		}
		token, err := GenerateJWT(username, jwtSecret)
		if err != nil {
			logutil.Error("生成 token 失败", "err", err)
			failWrap(c, CodeTokenSignFailed, err, nil)
			return
		}
		c.JSON(http.StatusOK, APIResponse{Code: 200, Data: gin.H{"token": token}})
	}
}

func handleLogout() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(200, APIResponse{Code: 200, Data: nil})
	}
}
