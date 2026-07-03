package server

import (
	"net/http"
	"time"

	"good-review-master/cache"
	"good-review-master/config"

	"github.com/gin-gonic/gin"
)

// groupInfo 群信息展示结构
type groupInfo struct {
	GroupID      string
	MessageCount int
	LastActivity string
	Cached       bool
}

// groupsPageData 群列表页数据
type groupsPageData struct {
	Title       string
	BotNickname string
	BotQQ       string
	APIKey      string
	Groups      []groupInfo
}

// messagesPageData 消息详情页数据
type messagesPageData struct {
	Title       string
	BotNickname string
	BotQQ       string
	APIKey      string
	GroupID     string
	Messages    []cache.Message
}

func handleIndex(c *gin.Context) {
	c.Redirect(http.StatusFound, "/groups")
}

func makeHandleGroups(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		cachedIDs := cache.ListGroupIDs()
		cachedSet := make(map[string]struct{}, len(cachedIDs))
		for _, id := range cachedIDs {
			cachedSet[id] = struct{}{}
		}

		groups := make([]groupInfo, 0, len(cfg.AllowGroups))
		for _, groupID := range cfg.AllowGroups {
			info := groupInfo{
				GroupID: groupID,
				Cached:  false,
			}
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

		c.HTML(http.StatusOK, "groups.html", groupsPageData{
			Title:       "不是好评大师 - 群列表",
			BotNickname: cfg.BotNickname,
			BotQQ:       cfg.BotQQ,
			APIKey:      cfg.MaskedAPIKey(),
			Groups:      groups,
		})
	}
}

func makeHandleMessages(cfg *config.Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")

		gc := cache.GetCache(groupID)
		if gc == nil {
			c.HTML(http.StatusNotFound, "messages.html", messagesPageData{
				Title:       "不是好评大师 - 消息详情",
				BotNickname: cfg.BotNickname,
				BotQQ:       cfg.BotQQ,
				APIKey:      cfg.MaskedAPIKey(),
				GroupID:     groupID,
				Messages:    nil,
			})
			return
		}

		c.HTML(http.StatusOK, "messages.html", messagesPageData{
			Title:       "不是好评大师 - 消息详情",
			BotNickname: cfg.BotNickname,
			BotQQ:       cfg.BotQQ,
			APIKey:      cfg.MaskedAPIKey(),
			GroupID:     groupID,
			Messages:    gc.GetAll(),
		})
	}
}

// formatTimestamp 将 Unix 时间戳格式化为本地时间字符串
func formatTimestamp(ts int64) string {
	return time.Unix(ts, 0).Format("2006-01-02 15:04:05")
}

func makeHandleAPIGroups() gin.HandlerFunc {
	return func(c *gin.Context) {
		cachedIDs := cache.ListGroupIDs()
		groups := make([]gin.H, 0, len(cachedIDs))
		for _, id := range cachedIDs {
			entry := gin.H{"group_id": id}
			gc := cache.GetCache(id)
			if gc != nil {
				entry["message_count"] = gc.Len()
				msgs := gc.GetAll()
				if len(msgs) > 0 {
					entry["messages"] = msgs
				}
			}
			groups = append(groups, entry)
		}
		c.JSON(http.StatusOK, gin.H{"groups": groups})
	}
}

func makeHandleAPIMessages() gin.HandlerFunc {
	return func(c *gin.Context) {
		groupID := c.Param("id")
		gc := cache.GetCache(groupID)
		if gc == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "group not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"group_id": groupID,
			"messages": gc.GetAll(),
		})
	}
}
