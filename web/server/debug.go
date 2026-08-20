package server

import (
	"net/http"
	"sync"
	"time"

	"good-review-master/cache"
	"good-review-master/internal/testutil"
	"good-review-master/logutil"
	"good-review-master/onebot"

	"github.com/gin-gonic/gin"
)

// MessageRouter 供 debug trigger 触发指令路由（由 cmd.Router 实现，避免 web 依赖 cmd）
type MessageRouter interface {
	RouteMessage(content string, event onebot.Event, groupID string)
}

// debugState 测试模式的调试状态（msg_id 自增序列等）
type debugState struct {
	mu     sync.Mutex
	msgSeq int64
}

// EnableDebug 注册测试模式自测接口（仅 GOOD_REVIEW_TEST=1 时调用；无鉴权，生产不可达）
func (s *Server) EnableDebug(router MessageRouter, fakeLLM *testutil.FakeLLM) {
	state := &debugState{msgSeq: 1_000_000}
	debug := s.engine.Group("/api/debug")
	debug.POST("/inject", s.handleDebugInject(state))
	debug.POST("/reset", s.handleDebugReset(state, fakeLLM))
	debug.GET("/state", s.handleDebugState(fakeLLM))
	debug.POST("/trigger", s.handleDebugTrigger(router))
	logutil.Warn("测试模式 debug 接口已注册", "base", "/api/debug")
}

// debugMsg 注入的单条消息
type debugMsg struct {
	MsgID   int64  `json:"msg_id"`
	UserID  string `json:"user_id"`
	Nick    string `json:"nick"`
	Card    string `json:"card"`
	Content string `json:"content"`
	Time    int64  `json:"time"`
}

// handleDebugInject 向指定群注入消息填充缓存
func (s *Server) handleDebugInject(state *debugState) gin.HandlerFunc {
	return func(c *gin.Context) {
		logutil.Warn("调试接口被调用", "action", "inject")
		var body struct {
			GroupID   string     `json:"group_id"`
			GroupName string     `json:"group_name"`
			Messages  []debugMsg `json:"messages"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.GroupID == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "data": nil})
			return
		}
		if body.GroupName != "" {
			s.groupNamesMu.Lock()
			s.groupNames[body.GroupID] = body.GroupName
			s.groupNamesMu.Unlock()
		}

		gc := cache.GetGroupCache(body.GroupID, s.cfg.MaxCacheMsg)
		now := time.Now().Unix()
		for _, m := range body.Messages {
			msg := cache.Message{
				GroupID: body.GroupID,
				UserID:  m.UserID,
				Nick:    m.Nick,
				Card:    m.Card,
				Content: m.Content,
				Time:    m.Time,
			}
			if m.MsgID != 0 {
				msg.MsgID = m.MsgID
			} else {
				state.mu.Lock()
				state.msgSeq++
				msg.MsgID = state.msgSeq
				state.mu.Unlock()
			}
			if msg.UserID == "" {
				msg.UserID = "1"
			}
			if msg.Time == 0 {
				msg.Time = now
			}
			gc.Add(msg)
		}
		c.JSON(http.StatusOK, gin.H{"code": 200, "data": gin.H{"injected": len(body.Messages)}})
	}
}

// handleDebugReset 清空缓存、锚点、fake 状态与群名缓存
func (s *Server) handleDebugReset(state *debugState, fakeLLM *testutil.FakeLLM) gin.HandlerFunc {
	return func(c *gin.Context) {
		logutil.Warn("调试接口被调用", "action", "reset")
		cache.ResetAll()
		fakeLLM.Reset()
		state.mu.Lock()
		state.msgSeq = 1_000_000
		state.mu.Unlock()
		// 原地清空而非替换：handleAPIGroups 在 New() 时捕获了 map 引用，替换会使其读到旧 map
		s.groupNamesMu.Lock()
		for k := range s.groupNames {
			delete(s.groupNames, k)
		}
		s.groupNamesMu.Unlock()
		c.JSON(http.StatusOK, gin.H{"code": 200, "data": nil})
	}
}

// handleDebugState 返回缓存、锚点、FakeLLM 调用快照（供断言）
func (s *Server) handleDebugState(fakeLLM *testutil.FakeLLM) gin.HandlerFunc {
	return func(c *gin.Context) {
		logutil.Warn("调试接口被调用", "action", "state")
		groups := make([]gin.H, 0)
		for _, gid := range cache.ListGroupIDs() {
			gc := cache.GetCache(gid)
			if gc == nil {
				continue
			}
			groups = append(groups, gin.H{
				"group_id":      gid,
				"cached":        gc.Len() > 0,
				"message_count": gc.Len(),
				"messages":      gc.GetAll(),
			})
		}
		c.JSON(http.StatusOK, gin.H{
			"code": 200,
			"data": gin.H{
				"groups":    groups,
				"anchors":   cache.ListAnchors(),
				"llm_calls": fakeLLM.Calls(),
			},
		})
	}
}

// handleDebugTrigger 模拟 @机器人 消息，走完整指令路由（handler 异步执行，立即返回）
func (s *Server) handleDebugTrigger(router MessageRouter) gin.HandlerFunc {
	return func(c *gin.Context) {
		logutil.Warn("调试接口被调用", "action", "trigger")
		var body struct {
			GroupID string `json:"group_id"`
			Content string `json:"content"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.GroupID == "" || body.Content == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "data": nil})
			return
		}
		event := onebot.Event{
			PostType:    "message",
			MessageType: "group",
			GroupID:     body.GroupID,
			UserID:      "999",
			Nickname:    "测试用户",
			RawMessage:  body.Content,
			MessageID:   time.Now().UnixNano(),
		}
		router.RouteMessage(body.Content, event, body.GroupID)
		c.JSON(http.StatusOK, gin.H{"code": 200, "data": gin.H{"queued": true}})
	}
}
