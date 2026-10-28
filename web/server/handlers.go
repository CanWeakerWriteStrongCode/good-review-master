package server

import (
	"crypto/subtle"
	"net/http"
	"sort"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/workspace"

	"github.com/gin-gonic/gin"
)

// APIResponse 统一 API 响应格式
type APIResponse struct {
	Code int         `json:"code"`
	Data interface{} `json:"data"`
}

// GroupDetail 群信息
type GroupDetail struct {
	GroupID       string `json:"group_id"`
	GroupName     string `json:"group_name"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	SelfID        string `json:"self_id"`      // 接收该群的机器人账号；空=无法归属
	AccountName   string `json:"account_name"` // 账号昵称；解析不到时为空
	MessageCount  int    `json:"message_count"`
	LastActivity  string `json:"last_activity"`
	Cached        bool   `json:"cached"`
}

// BotStatus Bot 运行时状态
type BotStatus struct {
	BotNickname    string `json:"bot_nickname"`
	APIKey         string `json:"api_key"`
	GroupCount     int    `json:"group_count"`
	AccountCount   int    `json:"account_count"`
	AllowAllGroups bool   `json:"allow_all_groups"`
}

// panelGroupIDs 返回管理面板应展示的群列表。
// 配置了通配符 "*" 时没有固定群号可列，只能展示已经产生缓存消息的群。
func panelGroupIDs(cfg *config.Config) []string {
	if !cfg.AllowsAllGroups() {
		return cfg.AllowGroups
	}
	ids := cache.ListGroupIDs()
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
				c.JSON(http.StatusBadRequest, APIResponse{Code: 400, Data: gin.H{
					"msg": "存在多个工作空间，请使用 /api/workspaces/{id}/groups 指定账号",
				}})
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
				"bot_info": s.botStatus(cfg, len(groups), len(accounts)),
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
		APIKey:         cfg.MaskedAPIKey(),
		GroupCount:     groupCount,
		AccountCount:   accountCount,
		AllowAllGroups: cfg.AllowsAllGroups(),
	}
}

// handleAPIWorkspaces 返回工作空间列表与各自汇总。
func (s *Server) handleAPIWorkspaces() gin.HandlerFunc {
	return func(c *gin.Context) {
		views := s.workspaceViews()
		list := make([]WorkspaceInfo, 0, len(views))
		groupCount, accountCount := 0, 0
		for _, view := range views {
			info := s.workspaceInfo(view)
			list = append(list, info)
			groupCount += info.GroupCount
			accountCount += len(accountIDsFor(view.SelfIDs, view.Store, view.Store.ListGroupIDs()))
		}
		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: gin.H{
				"workspaces": list,
				"bot_info":   s.botStatus(s.cfg(), groupCount, accountCount),
			},
		})
	}
}

// handleAPIWorkspaceGroups 返回指定工作空间的群与账号汇总。
func (s *Server) handleAPIWorkspaceGroups() gin.HandlerFunc {
	return func(c *gin.Context) {
		view, ok := s.workspaceView(c.Param("wid"))
		if !ok {
			c.JSON(http.StatusNotFound, APIResponse{Code: 404, Data: gin.H{"msg": "工作空间不存在"}})
			return
		}
		groups, accounts := s.collectWorkspaceDetails(view)
		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: gin.H{
				"workspace": s.workspaceInfo(view),
				"groups":    groups,
				"accounts":  accounts,
				"bot_info":  s.botStatus(s.cfg(), len(groups), len(accounts)),
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
		store := cache.DefaultStore()
		var wsID, wsName string
		if views := s.workspaceViews(); len(views) > 0 {
			if len(views) != 1 {
				c.JSON(http.StatusBadRequest, APIResponse{Code: 400, Data: gin.H{
					"msg": "存在多个工作空间，请使用 /api/workspaces/{id}/groups/{group_id} 指定账号",
				}})
				return
			}
			store, wsID, wsName = views[0].Store, views[0].ID, views[0].Name
		}
		s.writeMessages(c, c.Param("id"), store, wsID, wsName)
	}
}

// handleAPIWorkspaceMessages 返回指定工作空间下某个群的消息。
func (s *Server) handleAPIWorkspaceMessages() gin.HandlerFunc {
	return func(c *gin.Context) {
		view, ok := s.workspaceView(c.Param("wid"))
		if !ok {
			c.JSON(http.StatusNotFound, APIResponse{Code: 404, Data: gin.H{"msg": "工作空间不存在"}})
			return
		}
		s.writeMessages(c, c.Param("gid"), view.Store, view.ID, view.Name)
	}
}

func (s *Server) writeMessages(c *gin.Context, groupID string, store *cache.Store, wsID, wsName string) {
	s.groupNamesMu.RLock()
	groupName := s.groupNames[groupID]
	s.groupNamesMu.RUnlock()

	gc := storeOr(store).GetCache(groupID)
	if gc == nil {
		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: gin.H{
				"group_id":       groupID,
				"group_name":     groupName,
				"workspace_id":   wsID,
				"workspace_name": wsName,
				"self_id":        "",
				"account_name":   "",
				"messages":       []cache.Message{},
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
			"workspace_id":   wsID,
			"workspace_name": wsName,
			"self_id":        stats.SelfID,
			"account_name":   s.accountName(stats.SelfID),
			"messages":       gc.GetAll(),
			"empty":          stats.Count == 0,
		},
	})
}

func (s *Server) handleAPIStatus() gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := s.cfg()
		groupCount := len(panelGroupIDs(cfg))
		accountCount := len(accountIDs(cfg))
		if views := s.workspaceViews(); len(views) > 0 {
			groupCount = 0
			accountCount = 0
			for _, view := range views {
				groups, accounts := s.collectWorkspaceDetails(view)
				groupCount += len(groups)
				accountCount += len(accounts)
			}
		}
		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: s.botStatus(cfg, groupCount, accountCount),
		})
	}
}

// formatTimestamp 将 Unix 时间戳格式化为本地时间字符串
func formatTimestamp(ts int64) string {
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
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
			c.Header("Retry-After", "60")
			c.JSON(http.StatusTooManyRequests, APIResponse{Code: 429, Data: gin.H{"msg": "尝试过于频繁，请稍后再试"}})
			return
		}

		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, APIResponse{Code: 400, Data: nil})
			return
		}
		// 常量时间比较：避免用响应时间逐字节猜密码（subtle 的用法见 chacha20 文档）
		if subtle.ConstantTimeCompare([]byte(body.Username), []byte(username)) != 1 ||
			subtle.ConstantTimeCompare([]byte(body.Password), []byte(password)) != 1 {
			limiter.RecordFailure(clientIP)
			logutil.Warn("登录失败", "ip", clientIP, "username", body.Username)
			c.JSON(http.StatusOK, APIResponse{Code: 401, Data: gin.H{"msg": "账号或密码错误"}})
			return
		}
		token, err := GenerateJWT(username, jwtSecret)
		if err != nil {
			logutil.Error("生成 token 失败", "err", err)
			c.JSON(http.StatusInternalServerError, APIResponse{Code: 500, Data: gin.H{"msg": "token 生成失败"}})
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
