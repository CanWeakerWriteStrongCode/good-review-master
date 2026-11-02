// Package speech 是发言域：谁在跟机器人说话、要不要回、怎么生成、怎么排版、怎么发。
//
// 一个 Router 服务一个工作空间。包内按角色分文件：
//
//	dispatch.go       薄入口：一条消息进来，命中指令就分发，没命中就走直答
//	command.go        指令表：前缀树 + 整表原子替换 + 热重载后重建
//	judge.go          判定：这条消息冲谁来、要不要主动接话
//	proactive.go      主动接话：裁判 + 每群串行 worker + 三重限流
//	generate.go       生成：选窗 → 拼 user 消息 → 调模型（有工具就进多轮）
//	reply_format.go   排版：把长回复拆成最多 N 句
//	deliver.go        交付：句子 → 发送动作 → 台账 → 聊天渠道
//	persona_render.go 人格：按群绑定、渲染成提示片段
//
// 依赖只朝下：只 import conversation、config、llm、onebot、mcpserver 这些中低层包，
// 不 import bot、pipeline、web。
package speech

import (
	"context"
	"sync"
	"sync/atomic"

	"good-review-master/async"
	"good-review-master/config"
	"good-review-master/conversation"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

// Router 指令路由器
type Router struct {
	// table 是当前路由表，rebuild 时整体替换、分发时无锁读取。
	// 热重载让 rebuild 可能来自 informer goroutine，同时消息分发与内部指令的
	// 异步任务都在读它——必须原子替换，不能就地改。
	table atomic.Pointer[routeTable]

	// internalCommands 内部指令，只在启动时 register，之后只读。
	// 单独存一份而不是从 table.routes 里筛 Category=="internal"：
	// 那种做法要求"内部指令一定还在 routes 里"，是个隐式约定；
	// 分开存之后 rebuild 的输入是明确的，也不会随 table 的替换而丢失。
	internalCommands []Command

	handlerMap map[string]HandlerFunc
	llmClient  llm.Client
	obClient   *onebot.Client
	promptCfg  *config.PromptConfig
	appCfg     config.Snapshot
	mcp        MCPProvider // MCP 工具提供者，未启用时为 nil
	starter    *async.Group

	// store 是本工作空间的缓存与锚点。默认指向进程级默认 Store（单账号测试、
	// 旧调用点）；多工作空间部署由 workspace 包注入各自的 Store。
	store *conversation.Store
	// workspace 是工作空间 ID，只用于指标与日志标签（每个 Router 一个工作空间）。
	workspace string

	// groupPersona 各群当前人格（#切换人格/#取消人格 维护，纯 @ 聊天时生效）。
	// 同一分发路径读写，加锁防未来并发扩展。
	groupPersonaMu sync.RWMutex
	groupPersona   map[string]*groupPersonaBinding // groupID → 本群当前人格

	// proactive 主动插话的每群状态。键是 groupID，但因为 Router 每个工作空间一个，
	// 实际语义是 (workspace, group) 联合——同一个群里的两个机器人互不阻塞。
	proactive proactiveRegistry

	// personaStore 可选：把群人格的**源关键字**持久化到共享 state.db（带 workspace_id）。
	// 只存关键字而不是渲染后的文本：提示词热更新后要按新版本重新绑定。
	personaStore PersonaStore

	// displayName 按群查成员显示名（工作空间的成员目录）。nil 时日志与 handler
	// 只用 user_id，不影响功能——昵称只是给人看的。
	displayName func(groupID, userID string) string

	// saveMessage 把消息写进共享 state.db（工作空间的 State.SaveMessage），返回行号。
	// 机器人自己的回复也走这里；nil 时只写内存缓存，重启后丢失。
	saveMessage func(conversation.Message) int64

	// saveAnchor 把选窗锚点写进共享 state.db（工作空间的 State.SaveAnchor）。
	// 不写的话锚点只活在内存：每次重启后每个群的第一条消息都得重新建立前缀缓存。
	// nil 时只写内存，行为与接入前一致。
	saveAnchor func(groupID string, anchor conversation.LLMAnchor)

	// ledger 发送台账：动作编号为键，重试时跳过已发动作。
	ledger SendLedger
}

// PersonaStore 持久化群当前人格。由工作空间的 state.Store 实现。
type PersonaStore interface {
	SaveGroupPersona(groupID, keyword string)
	DeleteGroupPersona(groupID string)
}

// NewRouter 创建路由器并初始化所有内部指令。
// mcpProvider 可传 nil（MCP 未启用），此时对话一律走单轮无工具调用。
func NewRouter(appCfg config.Snapshot, promptCfg *config.PromptConfig, llmClient llm.Client, obClient *onebot.Client, mcpProvider MCPProvider, shutdownCtx context.Context) *Router {
	r := &Router{
		llmClient:    llmClient,
		obClient:     obClient,
		promptCfg:    promptCfg,
		appCfg:       appCfg,
		mcp:          mcpProvider,
		store:        conversation.DefaultStore(),
		starter:      async.New(shutdownCtx),
		groupPersona: make(map[string]*groupPersonaBinding),
		ledger:       NewMemoryLedger(),
	}
	r.handlerMap = map[string]HandlerFunc{
		"chat_review": r.chatReview,
	}
	r.registerInternalCommands()
	r.Rebuild()
	return r
}

// SetStore 注入本工作空间的缓存与锚点。必须在开始处理消息前调用。
func (r *Router) SetStore(store *conversation.Store) {
	if store != nil {
		r.store = store
	}
}

// SetWorkspace 设置工作空间 ID，只影响指标与日志标签。
func (r *Router) SetWorkspace(id string) { r.workspace = id }

// SetMessageSink 注入消息持久化入口（工作空间的 State.SaveMessage）：
// 机器人回复也要落库，重启后聊天记录里才还有自己说过的话。
// 返回值是该 message 在 state.db 里的行号，写入失败返回 0（这条就不带 message_id）。
func (r *Router) SetMessageSink(sink func(conversation.Message) int64) { r.saveMessage = sink }

// SetAnchorSink 注入锚点持久化入口（工作空间的 State.SaveAnchor）。
// 锚点记的是"上次发出去的窗口从哪条到哪条"，落库之后进程重启也能接着往后加，
// 不用每个群重新攒一遍前缀。nil 时锚点只留在内存。
func (r *Router) SetAnchorSink(sink func(groupID string, anchor conversation.LLMAnchor)) {
	r.saveAnchor = sink
}

// SetSendLedger 注入发送台账（state.db 实现）。nil 时保持进程内台账。
func (r *Router) SetSendLedger(ledger SendLedger) {
	if ledger != nil {
		r.ledger = ledger
	}
}

// SetDisplayName 注入成员显示名查询（工作空间成员目录）。必须在分发前调用。
func (r *Router) SetDisplayName(fn func(groupID, userID string) string) { r.displayName = fn }

// SetPersonaStore 注入人格持久化。必须在开始处理消息前调用。
func (r *Router) SetPersonaStore(store PersonaStore) { r.personaStore = store }

// cacheStore 返回注入的 Store；未注入时退回进程级默认 Store。
func (r *Router) cacheStore() *conversation.Store {
	if r.store == nil {
		return conversation.DefaultStore()
	}
	return r.store
}

// WorkspaceID 返回本 Router 所属工作空间（测试与面板用）。
func (r *Router) WorkspaceID() string { return r.workspace }

// nameOf 返回成员显示名；没有目录时返回空串，调用方自行回退。
func (r *Router) nameOf(groupID, userID string) string {
	if r.displayName == nil {
		return ""
	}
	return r.displayName(groupID, userID)
}

// Go 安全启动 goroutine（代理 async.Group）
func (r *Router) Go(fn func(context.Context) error) {
	r.starter.Go(fn)
}

// GoCtx 在调用方 ctx 基础上安全启动 goroutine（代理 async.Group.GoCtx）。
// 分发链路上的异步任务用这个，才能把 trace span 带进 goroutine。
func (r *Router) GoCtx(parent context.Context, fn func(context.Context) error) {
	r.starter.GoCtx(parent, fn)
}

// Wait 等待所有 goroutine 完成（代理 async.Group）
func (r *Router) Wait() error {
	return r.starter.Wait()
}

// getGroupPersona 返回某群当前人格快照（#切换人格 设置）
func (r *Router) getGroupPersona(groupID string) (*groupPersonaBinding, bool) {
	r.groupPersonaMu.RLock()
	defer r.groupPersonaMu.RUnlock()
	b, ok := r.groupPersona[groupID]
	return b, ok
}

// setGroupPersona 记录某群当前人格
func (r *Router) setGroupPersona(groupID string, b *groupPersonaBinding) {
	r.groupPersonaMu.Lock()
	r.groupPersona[groupID] = b
	r.groupPersonaMu.Unlock()
	if r.personaStore != nil && b != nil {
		r.personaStore.SaveGroupPersona(groupID, b.keyword)
	}
}

// clearGroupPersona 清空某群当前人格（#取消人格）
func (r *Router) clearGroupPersona(groupID string) {
	r.groupPersonaMu.Lock()
	delete(r.groupPersona, groupID)
	r.groupPersonaMu.Unlock()
	if r.personaStore != nil {
		r.personaStore.DeleteGroupPersona(groupID)
	}
}

// RestorePersonas 把 state.db 里本工作空间存的关键字重新绑定成当前路由表里的人格。
// 关键字在提示词里已经不存在时降级为默认人格并告警——提示词被人改过是常态。
func (r *Router) RestorePersonas(stored map[string]string) {
	for groupID, keyword := range stored {
		binding := r.personaBinding(keyword)
		if binding == nil {
			logutil.Warn("持久化的群人格关键字已不存在，按默认人格处理", "group", groupID, "keyword", keyword)
			if r.personaStore != nil {
				r.personaStore.DeleteGroupPersona(groupID)
			}
			continue
		}
		r.setGroupPersona(groupID, binding)
	}
}

// personaBinding 按关键字找一条自带人格的路由，复制成自包含绑定。
func (r *Router) personaBinding(keyword string) *groupPersonaBinding {
	for _, route := range r.routesSnapshot() {
		if route.Keyword == keyword && route.Persona != nil {
			return &groupPersonaBinding{
				keyword:     keyword,
				persona:     *route.Persona,
				sharedRules: route.SharedRules,
			}
		}
	}
	return nil
}

// availablePersonaNames 列出所有"自带人格"的关键字（可作为 #切换人格 的目标人格）。
// 直接遍历 rebuild 好的路由表：新增/删除关键字经 Reload()+rebuild() 后自动同步。
func (r *Router) availablePersonaNames() []string {
	var names []string
	for _, route := range r.routesSnapshot() {
		if route.Persona != nil {
			names = append(names, route.Keyword)
		}
	}
	return names
}
