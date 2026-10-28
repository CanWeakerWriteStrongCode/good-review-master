package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"good-review-master/cache"
	"good-review-master/internal/testutil"
	"good-review-master/logutil"
	"good-review-master/onebot"

	"github.com/gin-gonic/gin"
)

// MessageRouter 供 debug trigger 触发指令路由（由 router.Router 实现，避免 web 依赖 router）
type MessageRouter interface {
	RouteMessage(ctx context.Context, content string, event onebot.Event, groupID string)
}

// debugStore 选择调试接口要操作的缓存：指定工作空间 > 默认工作空间 > 进程级默认。
func (s *Server) debugStore(workspaceID string) (*cache.Store, string) {
	if s.workspaces == nil {
		return cache.DefaultStore(), ""
	}
	if workspaceID != "" {
		if view, ok := s.workspaces.ViewByID(workspaceID); ok {
			return view.Store, view.ID
		}
	}
	if def := s.workspaces.Default(); def != nil {
		return def.Cache, def.ID
	}
	return cache.DefaultStore(), ""
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
			WorkspaceID string     `json:"workspace_id"`
			GroupID     string     `json:"group_id"`
			GroupName   string     `json:"group_name"`
			Messages    []debugMsg `json:"messages"`
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

		store, wsID := s.debugStore(body.WorkspaceID)
		selfID := ""
		if s.workspaces != nil {
			if view, ok := s.workspaces.ViewByID(wsID); ok && len(view.SelfIDs) > 0 {
				selfID = view.SelfIDs[0]
			}
		}
		gc := store.GetGroupCache(body.GroupID, s.cfg().MaxCacheMsg)
		now := time.Now().Unix()
		for _, m := range body.Messages {
			msg := cache.Message{
				GroupID:  body.GroupID,
				SelfID:   selfID,
				UserID:   m.UserID,
				Nickname: m.Nick,
				Card:     m.Card,
				Content:  m.Content,
				Time:     m.Time,
			}
			if m.MsgID != 0 {
				msg.MessageID = m.MsgID
			} else {
				state.mu.Lock()
				state.msgSeq++
				msg.MessageID = state.msgSeq
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
		if s.workspaces != nil {
			s.workspaces.ResetAll()
		} else {
			cache.ResetAll()
		}
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
		anchors := make(map[string]cache.LLMAnchor)
		multiWorkspace := s.workspaces != nil && len(s.workspaces.Views()) > 1
		appendStore := func(wsID string, store *cache.Store) {
			for _, gid := range store.ListGroupIDs() {
				gc := store.GetCache(gid)
				if gc == nil {
					continue
				}
				groups = append(groups, gin.H{
					"workspace_id":  wsID,
					"group_id":      gid,
					"cached":        gc.Len() > 0,
					"message_count": gc.Len(),
					"messages":      gc.GetAll(),
				})
			}
			for group, anchor := range store.ListAnchors() {
				// 单工作空间保持旧的裸 group 键，兼容现有 E2E 与调试脚本；
				// 多工作空间才加前缀区分账号。
				key := group
				if multiWorkspace {
					key = wsID + "/" + group
				}
				anchors[key] = anchor
			}
		}
		if s.workspaces != nil {
			for _, view := range s.workspaces.Views() {
				appendStore(view.ID, view.Store)
			}
		} else {
			appendStore("", cache.DefaultStore())
		}
		c.JSON(http.StatusOK, gin.H{
			"code": 200,
			"data": gin.H{
				"groups":    groups,
				"anchors":   anchors,
				"llm_calls": fakeLLM.Calls(),
			},
		})
	}
}

// handleDebugTrigger 模拟 @机器人 消息，走完整指令路由。
// handler 经 async 池异步执行，这里阻塞等待其收尾（锚点更新）再返回：
// 否则异步副作用（SetLLMAnchor）可能在用例结束或下一次 reset 之后才落地，污染跨用例的锚点状态。
func (s *Server) handleDebugTrigger(router MessageRouter) gin.HandlerFunc {
	return func(c *gin.Context) {
		logutil.Warn("调试接口被调用", "action", "trigger")
		var body struct {
			WorkspaceID string `json:"workspace_id"`
			GroupID     string `json:"group_id"`
			Content     string `json:"content"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.GroupID == "" || body.Content == "" {
			c.JSON(http.StatusBadRequest, gin.H{"code": 400, "data": nil})
			return
		}
		store, resolvedWS := s.debugStore(body.WorkspaceID)
		if body.WorkspaceID != "" && resolvedWS != body.WorkspaceID {
			c.JSON(http.StatusNotFound, gin.H{"code": 404, "data": gin.H{"msg": "工作空间不存在"}})
			return
		}
		event := onebot.Event{
			PostType:    "message",
			MessageType: "group",
			GroupID:     body.GroupID,
			UserID:      "999",
			Sender:      onebot.Sender{Nickname: "测试用户"},
			RawMessage:  body.Content,
			MessageID:   time.Now().UnixNano(),
		}
		anchorBefore := store.GetLLMAnchor(body.GroupID)
		// 用请求的 ctx：debug trigger 触发的这条链路会与 HTTP 入口的 span 接上，
		// 于是 /api/debug/trigger 到一次大模型调用能在同一条 trace 里看完。
		router.RouteMessage(c.Request.Context(), body.Content, event, body.GroupID)
		// 等 handler 完全收尾：锚点已存在（首次触发）或 LastSent 已前进（重复触发）。
		// 超时（如空缓存不设锚点）则直接返回，由测试侧轮询兜底。
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			anchor := store.GetLLMAnchor(body.GroupID)
			if anchor != nil {
				if anchorBefore == nil || anchor.LastSent != anchorBefore.LastSent {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
		c.JSON(http.StatusOK, gin.H{"code": 200, "data": gin.H{"queued": true}})
	}
}
