package server

import (
	"crypto/subtle"
	"net/http"
	"sync"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/onebot"

	"github.com/gin-gonic/gin"
)

// APIResponse 统一 API 响应格式
type APIResponse struct {
	Code int         `json:"code"`
	Data interface{} `json:"data"`
}

// GroupDetail 群信息
type GroupDetail struct {
	GroupID      string `json:"group_id"`
	GroupName    string `json:"group_name"`
	MessageCount int    `json:"message_count"`
	LastActivity string `json:"last_activity"`
	Cached       bool   `json:"cached"`
}

// BotStatus Bot 运行时状态
type BotStatus struct {
	BotQQ       string `json:"bot_qq"`
	BotNickname string `json:"bot_nickname"`
	APIKey      string `json:"api_key"`
	GroupCount  int    `json:"group_count"`
}

func handleAPIGroups(snapshot config.Snapshot, obClient *onebot.Client, groupNames map[string]string, groupNamesMu *sync.RWMutex) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 一个请求内取一次快照：群列表与 bot 信息必须来自同一份配置，
		// 否则热更新插在中间会返回"群列表是新的、群数量是旧的"这种自相矛盾的响应
		cfg := snapshot()
		cachedIDs := cache.ListGroupIDs()
		cachedSet := make(map[string]struct{}, len(cachedIDs))
		for _, id := range cachedIDs {
			cachedSet[id] = struct{}{}
		}

		groups := make([]GroupDetail, 0, len(cfg.AllowGroups))
		for _, groupID := range cfg.AllowGroups {
			info := GroupDetail{
				GroupID: groupID,
				Cached:  false,
			}
			// 获取群名称（优先用缓存，否则调 NapCat API）
			groupNamesMu.RLock()
			name, ok := groupNames[groupID]
			groupNamesMu.RUnlock()
			if !ok && obClient != nil {
				gi, err := obClient.GetGroupInfo(groupID)
				if err == nil && gi.GroupName != "" {
					name = gi.GroupName
					groupNamesMu.Lock()
					groupNames[groupID] = name
					groupNamesMu.Unlock()
				}
			}
			info.GroupName = name
			if _, ok := cachedSet[groupID]; ok {
				info.Cached = true
				gc := cache.GetCache(groupID)
				if gc != nil {
					info.MessageCount = gc.Len()
					msgs := gc.GetAll()
					if len(msgs) > 0 {
						lastTime := msgs[len(msgs)-1].Time
						if lastTime > 0 {
							info.LastActivity = formatTimestamp(lastTime)
						}
					}
				}
			}
			groups = append(groups, info)
		}

		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: gin.H{
				"groups": groups,
				"bot_info": BotStatus{
					BotQQ:       cfg.BotQQ,
					BotNickname: cfg.BotNickname,
					APIKey:      cfg.MaskedAPIKey(),
					GroupCount:  len(cfg.AllowGroups),
				},
			},
		})
	}
}

// handleAPIMessages 返回指定群的缓存消息。
// groupNames 由 handleAPIGroups 与 debug inject/reset 并发读写，读也要持锁——
// 之前这里直读 map，是已知的数据竞争点。
func handleAPIMessages(groupNames map[string]string, groupNamesMu *sync.RWMutex) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		groupNamesMu.RLock()
		groupName := groupNames[groupID]
		groupNamesMu.RUnlock()

		gc := cache.GetCache(groupID)
		if gc == nil {
			c.JSON(http.StatusOK, APIResponse{
				Code: 200,
				Data: gin.H{
					"group_id":   groupID,
					"group_name": groupName,
					"messages":   []cache.Message{},
					"empty":      true,
				},
			})
			return
		}

		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: gin.H{
				"group_id":   groupID,
				"group_name": groupName,
				"messages":   gc.GetAll(),
				"empty":      gc.Len() == 0,
			},
		})
	}
}

func handleAPIStatus(snapshot config.Snapshot) gin.HandlerFunc {
	return func(c *gin.Context) {
		cfg := snapshot()
		c.JSON(http.StatusOK, APIResponse{
			Code: 200,
			Data: BotStatus{
				BotQQ:       cfg.BotQQ,
				BotNickname: cfg.BotNickname,
				APIKey:      cfg.MaskedAPIKey(),
				GroupCount:  len(cfg.AllowGroups),
			},
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
