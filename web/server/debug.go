package server

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"good-review-master/conversation"
	"good-review-master/internal/testutil"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/state"
	"good-review-master/workspace"

	"github.com/gin-gonic/gin"
)

// debugView 选调试接口要操作的工作空间：指定 ID > 默认工作空间。
// 拿不到（没启用工作空间）时返回 false，此时注入只进进程级缓存、不落库。
func (s *Server) debugView(workspaceID string) (workspace.View, bool) {
	if s.workspaces == nil {
		return workspace.View{}, false
	}
	if workspaceID != "" {
		return s.workspaces.ViewByID(workspaceID)
	}
	if def := s.workspaces.Default(); def != nil {
		return s.workspaces.ViewByID(def.ID)
	}
	return workspace.View{}, false
}

// debugState 测试模式的调试状态（msg_id 自增序列等）
type debugState struct {
	mu     sync.Mutex
	msgSeq int64
}

// EnableDebug 注册测试模式自测接口（仅 GOOD_REVIEW_TEST=1 时调用；无鉴权，生产不可达）
func (s *Server) EnableDebug(fakeLLM *testutil.FakeLLM) {
	state := &debugState{msgSeq: 1_000_000}
	debug := s.engine.Group("/api/debug")
	debug.POST("/inject", s.handleDebugInject(state))
	debug.POST("/reset", s.handleDebugReset(state, fakeLLM))
	debug.GET("/state", s.handleDebugState(fakeLLM))
	debug.POST("/trigger", s.handleDebugTrigger())
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
//
// dbg 这个名字不能叫 state：本函数要用 state 包里的 MemberInfo。
func (s *Server) handleDebugInject(dbg *debugState) gin.HandlerFunc {
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

		// 面板只从 state.db 读数，注入的数据必须落库，否则接口返回后页面上看不到。
		view, hasWorkspace := s.debugView(body.WorkspaceID)
		store := conversation.DefaultStore()
		if hasWorkspace {
			store = view.Store
		}
		selfID := view.Account
		if hasWorkspace && view.State != nil && body.GroupName != "" {
			view.State.SaveGroup(body.GroupID, body.GroupName)
		}
		gc := store.GetGroupCache(body.GroupID, s.cfg().MaxCacheMsg)
		now := time.Now().Unix()
		for _, m := range body.Messages {
			msg := conversation.Message{
				GroupID:  body.GroupID,
				SelfID:   selfID,
				UserID:   m.UserID,
				Nickname: m.Nick,
				Card:     m.Card,
				Content:  m.Content,
				Time:     m.Time,
			}
			if m.MsgID != 0 {
				msg.MessageID = strconv.FormatInt(m.MsgID, 10)
			} else {
				dbg.mu.Lock()
				dbg.msgSeq++
				msg.MessageID = strconv.FormatInt(dbg.msgSeq, 10)
				dbg.mu.Unlock()
			}
			if msg.UserID == "" {
				msg.UserID = "1"
			}
			if msg.Time == 0 {
				msg.Time = now
			}
			gc.Add(msg)
			if hasWorkspace && view.State != nil {
				view.State.SaveMessage(msg)
				// 身份存在 groups.self_user_id 里，真实链路由 LearnIdentity 写。
				// 测试注入没有那条链路，这里补一次，否则面板拿不到账号分组用的 self_id。
				if msg.SelfID != "" {
					view.State.SaveIdentity(body.GroupID, msg.SelfID)
				}
				// 成员表同理：真实链路在处理消息时写，注入跳过了 pipeline，
				// 这里补一次，否则面板「已收录成员」永远是 0。
				_, _ = view.State.SaveMemberInfo(state.MemberInfo{
					GroupID: body.GroupID, UserID: msg.UserID,
					Name: msg.Nickname, DisplayName: msg.Card,
				})
			}
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
			conversation.ResetAll()
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
		anchors := make(map[string]conversation.LLMAnchor)
		multiWorkspace := s.workspaces != nil && len(s.workspaces.Views()) > 1
		appendStore := func(wsID string, store *conversation.Store) {
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
			appendStore("", conversation.DefaultStore())
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
func (s *Server) handleDebugTrigger() gin.HandlerFunc {
	return func(c *gin.Context) {
		logutil.Warn("调试接口被调用", "action", "trigger")
		var body struct {
			WorkspaceID string `json:"workspace_id"`
			GroupID     string `json:"group_id"`
			Content     string `json:"content"`
		}
		if err := c.ShouldBindJSON(&body); err != nil || body.GroupID == "" || body.Content == "" {
			fail(c, CodeBadJSON, nil)
			return
		}
		if s.workspaces == nil {
			fail(c, CodeNoWorkspace, nil)
			return
		}
		// 工作空间是收到第一条事件才开通的，不能在 Server 构造时就把 Router 定死：
		// 每次调用按 workspace_id（缺省时要求只剩一个）现查。
		var ws *workspace.Workspace
		if body.WorkspaceID != "" {
			found, ok := s.workspaces.ByID(body.WorkspaceID)
			if !ok {
				fail(c, CodeWorkspaceNotFound, nil)
				return
			}
			ws = found
		} else {
			ws = s.workspaces.Default()
			if ws == nil {
				fail(c, CodeMultipleWorkspaces, map[string]any{"Endpoint": "workspace_id"})
				return
			}
		}

		event := onebot.Event{
			Type:       "message",
			DetailType: "group",
			GroupID:    body.GroupID,
			UserID:     "999",
			Self:       onebot.Self{Platform: ws.Platform, UserID: "test_self"},
			Message:    []onebot.Segment{onebot.TextSegment(body.Content)},
			AltMessage: body.Content,
			MessageID:  strconv.FormatInt(time.Now().UnixNano(), 10),
		}
		anchorBefore := ws.Cache.GetLLMAnchor(body.GroupID)
		// 用请求的 ctx：debug trigger 触发的这条链路会与 HTTP 入口的 span 接上，
		// 于是 /api/debug/trigger 到一次大模型调用能在同一条 trace 里看完。
		ws.Router.RouteMessage(c.Request.Context(), body.Content, event, body.GroupID)
		// 等 handler 完全收尾：锚点已存在（首次触发）或 LastSent 已前进（重复触发）。
		// 超时（如空缓存不设锚点）则直接返回，由测试侧轮询兜底。
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			anchor := ws.Cache.GetLLMAnchor(body.GroupID)
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
