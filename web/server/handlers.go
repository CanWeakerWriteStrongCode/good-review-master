package server

import (
	"net/http"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/onebot"

	"github.com/gin-gonic/gin"
)

// APIResponse 统一 API 响应格式
type APIResponse struct {
	Code int         `json:"code"`
	Data interface{} `json:"data"`
}

// GroupInfo 群信息
type GroupInfo struct {
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

func handleAPIGroups(cfg *config.Config, obClient *onebot.Client, groupNames map[string]string) gin.HandlerFunc {
	return func(c *gin.Context) {
		cachedIDs := cache.ListGroupIDs()
		cachedSet := make(map[string]struct{}, len(cachedIDs))
		for _, id := range cachedIDs {
			cachedSet[id] = struct{}{}
		}

		groups := make([]GroupInfo, 0, len(cfg.AllowGroups))
		for _, groupID := range cfg.AllowGroups {
			info := GroupInfo{
				GroupID: groupID,
				Cached:  false,
			}
			// 获取群名称（优先用缓存，否则调 NapCat API）
			name, ok := groupNames[groupID]
			if !ok && obClient != nil {
				gi, err := obClient.GetGroupInfo(groupID)
				if err == nil && gi.GroupName != "" {
					name = gi.GroupName
					groupNames[groupID] = name
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

func handleAPIMessages(cfg *config.Config, obClient *onebot.Client, groupNames map[string]string) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		groupName := groupNames[groupID]

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

func handleAPIStatus(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
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

// handleLogin 登录校验，成功返回 token
func handleLogin(username, password string, tokens *TokenStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		if password == "" {
			c.JSON(200, APIResponse{Code: 200, Data: gin.H{"need_password": false}})
			return
		}
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, APIResponse{Code: 400, Data: nil})
			return
		}
		if body.Username != username || body.Password != password {
			c.JSON(200, APIResponse{Code: 401, Data: gin.H{"msg": "账号或密码错误"}})
			return
		}
		token := tokens.Generate()
		c.JSON(200, APIResponse{Code: 200, Data: gin.H{"token": token}})
	}
}

func handleLogout(tokens *TokenStore) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := c.GetHeader("Authorization")
		if len(token) > 7 && token[:7] == "Bearer " {
			tokens.Remove(token[7:])
		}
		c.JSON(200, APIResponse{Code: 200, Data: nil})
	}
}
