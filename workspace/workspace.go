// Package workspace 管理多机器人账号的工作空间。
//
// 一个工作空间 = 一个机器人账号的全部可观察状态：消息缓存、选窗锚点、群人格、
// 指令表与提示词、身份昵称。各工作空间之间不共享任何运行时状态；
// OneBot 客户端、MCP 连接、大模型客户端等外部资源仍由组合根共享。
package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/router"
	"good-review-master/telemetry"
)

// Workspace 一个机器人账号的运行时。
type Workspace struct {
	ID         string
	PlatformID string
	SelfIDs    []string
	PromptDir  string

	Router *router.Router
	Prompt *config.PromptConfig
	Cache  *cache.Store

	appCfg     config.Snapshot
	nickname   string // 启动时从桥接解析到的昵称；配置 nickname 为空时使用
	systemPath string // 实际加载的系统提示词文件（工作空间覆盖或全局）
	customPath string // 实际写入的自定义提示词文件
}

// View 是面板用的只读视图（不含 Router/Prompt 等运行时对象）。
type View struct {
	ID         string
	Name       string
	PlatformID string
	SelfIDs    []string
	Groups     []string
	Store      *cache.Store
}

// Manager 持有全部工作空间与归属索引。
type Manager struct {
	appCfg     config.Snapshot
	list       []*Workspace
	byPlatform map[string]*Workspace
	bySelfID   map[string]*Workspace
	byNickname map[string]*Workspace
	single     *Workspace
}

// New 按配置创建工作空间。没有 bot.workspaces 时创建一个隐式 default，
// 提示词沿用全局文件，行为与单账号版本一致。
func New(appCfg config.Snapshot, systemPromptPath, customPromptPath string,
	llmClient llm.Client, obClient *onebot.Client, mcp router.MCPProvider,
	shutdownCtx context.Context) (*Manager, error) {

	cfg := appCfg()
	confs := cfg.EffectiveWorkspaces()
	m := &Manager{
		appCfg:     appCfg,
		byPlatform: make(map[string]*Workspace, len(confs)),
		bySelfID:   make(map[string]*Workspace, len(confs)),
		byNickname: make(map[string]*Workspace),
	}

	for i, conf := range confs {
		ws, err := newWorkspace(appCfg, cfg, conf, systemPromptPath, customPromptPath, llmClient, obClient, mcp, shutdownCtx)
		if err != nil {
			return nil, fmt.Errorf("初始化工作空间 %s 失败：%w", conf.ID, err)
		}
		m.list = append(m.list, ws)
		if ws.PlatformID != "" {
			m.byPlatform[ws.PlatformID] = ws
		}
		for _, selfID := range ws.SelfIDs {
			m.bySelfID[selfID] = ws
		}
		if name := ws.Snapshot()().BotNickname; name != "" {
			if _, exists := m.byNickname[name]; exists {
				// 昵称重复时不能用于 @ 兜底：否则一条 "@同名" 的消息无法确定归属。
				m.byNickname[name] = nil
			} else {
				m.byNickname[name] = ws
			}
		}
		if i == 0 {
			m.single = ws
		}
	}
	if len(m.list) == 1 {
		m.single = m.list[0]
	} else {
		m.single = nil
	}
	return m, nil
}

func newWorkspace(appCfg config.Snapshot, cfg *config.Config, conf config.WorkspaceConf,
	systemPromptPath, customPromptPath string,
	llmClient llm.Client, obClient *onebot.Client, mcp router.MCPProvider,
	shutdownCtx context.Context) (*Workspace, error) {

	promptDir := conf.PromptDir
	if promptDir == "" {
		promptDir = filepath.Join(filepath.Dir(systemPromptPath), "workspaces", conf.ID)
	}
	if err := os.MkdirAll(promptDir, 0755); err != nil {
		return nil, fmt.Errorf("创建工作空间提示词目录失败：%w", err)
	}
	wsSystem := filepath.Join(promptDir, "prompt_system.yaml")
	if _, err := os.Stat(wsSystem); err != nil {
		wsSystem = systemPromptPath
	}
	wsCustom := filepath.Join(promptDir, "prompt_custom.yaml")
	// 全局自定义文件在单账号（隐式 default）时继续沿用，避免升级后"自定义指令消失"。
	if len(cfg.Workspaces) == 0 {
		wsCustom = customPromptPath
	} else if _, err := os.Stat(wsCustom); err != nil {
		// 首次为账号创建工作空间：把现有全局自定义指令复制一份作为起点。
		// 之后各账号独立读写自己的文件，不会再互相影响。
		if raw, err := os.ReadFile(customPromptPath); err == nil {
			if err := os.WriteFile(wsCustom, raw, 0644); err != nil {
				return nil, fmt.Errorf("初始化工作空间自定义提示词失败：%w", err)
			}
		}
	}
	promptCfg, err := config.LoadPromptConfig(wsSystem, wsCustom)
	if err != nil {
		return nil, err
	}

	ws := &Workspace{
		ID:         conf.ID,
		PlatformID: conf.PlatformID,
		SelfIDs:    append([]string(nil), conf.SelfIDs...),
		PromptDir:  promptDir,
		Prompt:     promptCfg,
		Cache:      cache.NewStore(),
		systemPath: wsSystem,
		customPath: wsCustom,
	}
	ws.nickname = resolveNickname(cfg, conf, obClient)
	ws.appCfg = func() *config.Config { return ws.snapshot(appCfg()) }

	ws.Router = router.NewRouter(ws.appCfg, promptCfg, llmClient, obClient, mcp, shutdownCtx)
	ws.Router.SetStore(ws.Cache)
	ws.Router.SetWorkspace(ws.ID)
	return ws, nil
}

// Snapshot 返回带上工作空间覆盖的配置快照。
// 每次调用都从最新全局快照取，因此 nickname / allow_groups 的修改可以热生效。
func (w *Workspace) Snapshot() config.Snapshot {
	return w.appCfg
}

func (w *Workspace) snapshot(global *config.Config) *config.Config {
	cp := *global
	if conf, ok := global.WorkspaceByID(w.ID); ok {
		if conf.Nickname != "" {
			cp.BotNickname = conf.Nickname
		} else if w.nickname != "" {
			cp.BotNickname = w.nickname
		}
		if len(conf.AllowGroups) > 0 {
			cp.AllowGroups = conf.AllowGroups
		}
	} else if w.nickname != "" {
		cp.BotNickname = w.nickname
	}
	return &cp
}

// resolveNickname 取该账号的昵称：配置显式值 > 桥接按 self_id 查询 > 单工作空间时用全局昵称。
func resolveNickname(cfg *config.Config, conf config.WorkspaceConf, obClient *onebot.Client) string {
	if conf.Nickname != "" {
		return conf.Nickname
	}
	if obClient != nil {
		for _, selfID := range conf.SelfIDs {
			info, err := obClient.GetLoginInfoFor(selfID)
			if err != nil || info == nil || info.Nickname == "" || !info.Matches(selfID) {
				continue
			}
			return info.Nickname
		}
	}
	if len(cfg.Workspaces) == 0 {
		return cfg.BotNickname
	}
	return ""
}

// List 返回全部工作空间（顺序与配置一致）。
func (m *Manager) List() []*Workspace { return m.list }

// Default 返回默认工作空间：优先 ID=default，否则第一个。
// debug 接口与只支持单工作空间的旧调用方用它。
func (m *Manager) Default() *Workspace {
	for _, ws := range m.list {
		if ws.ID == "default" {
			return ws
		}
	}
	if len(m.list) > 0 {
		return m.list[0]
	}
	return nil
}

// ByID 按 ID 查找工作空间。
func (m *Manager) ByID(id string) (*Workspace, bool) {
	for _, ws := range m.list {
		if ws.ID == id {
			return ws, true
		}
	}
	return nil, false
}

// Resolve 解析事件所属工作空间。
//
// 顺序：platform_id（AstrBot 平台实例，恒定唯一）→ self_id 别名 → 单工作空间兜底。
// 多工作空间下解析不到时返回 nil：宁可这条消息不接管，也不能让错误的账号回复。
func (m *Manager) Resolve(event onebot.Event) *Workspace {
	if event.PlatformID != "" {
		if ws := m.byPlatform[event.PlatformID]; ws != nil {
			telemetry.WorkspaceResolveTotal.WithLabelValues("platform").Inc()
			return ws
		}
	}
	if selfID := event.BotID(); selfID != "" {
		if ws := m.bySelfID[selfID]; ws != nil {
			telemetry.WorkspaceResolveTotal.WithLabelValues("self_id").Inc()
			return ws
		}
	}
	if m.single != nil {
		telemetry.WorkspaceResolveTotal.WithLabelValues("single").Inc()
		return m.single
	}
	telemetry.WorkspaceResolveTotal.WithLabelValues("unresolved").Inc()
	return nil
}

// BindEvent 用工作空间已知身份补齐事件字段。
//
// 入站事件可能是占位 self_id（拿不到 mention 元数据），而出站动作代理要靠
// self_id 选平台实例；不补齐的话回复会被第一个适配器接走，多账号下就是回错账号。
func (w *Workspace) BindEvent(event onebot.Event) onebot.Event {
	if onebot.IsSelfIDPlaceholder(event.SelfID) && len(w.SelfIDs) > 0 {
		event.SelfID = w.SelfIDs[0]
	}
	if event.PlatformID == "" {
		event.PlatformID = w.PlatformID
	}
	return event
}

// AllowsGroup 判断群是否在该工作空间的白名单内（未覆盖时继承全局）。
func (w *Workspace) AllowsGroup(groupID string) bool {
	cfg := w.Snapshot()()
	for _, g := range cfg.AllowGroups {
		if g == "*" || g == groupID {
			return true
		}
	}
	return false
}

// AllowsAllGroups 判断该工作空间是否放行全部群。
func (w *Workspace) AllowsAllGroups() bool {
	for _, g := range w.Snapshot()().AllowGroups {
		if g == "*" {
			return true
		}
	}
	return false
}

// Nickname 返回当前生效的昵称。
func (w *Workspace) Nickname() string { return w.Snapshot()().BotNickname }

// Nil 表示该昵称唯一属于一个工作空间；只有这时才能用 "@昵称" 做归属兜底。
func (m *Manager) NicknameUnique(nickname string) bool {
	if nickname == "" {
		return false
	}
	return m.byNickname[nickname] != nil
}

// IsManagedAccount 判断 user_id 是否是任一工作空间的机器人账号。
// 用于把"另一个账号自己发的消息"挡在上下文之外。
func (m *Manager) IsManagedAccount(userID string) bool {
	if userID == "" {
		return false
	}
	for _, ws := range m.list {
		for _, id := range ws.SelfIDs {
			if id == userID {
				return true
			}
		}
	}
	return false
}

// Views 返回面板用的工作空间视图。
func (m *Manager) Views() []View {
	views := make([]View, 0, len(m.list))
	for _, ws := range m.list {
		conf := ws.Snapshot()()
		groups := append([]string(nil), conf.AllowGroups...)
		selfIDs := ws.SelfIDs
		if declared, ok := conf.WorkspaceByID(ws.ID); ok {
			if len(declared.AllowGroups) > 0 {
				groups = declared.AllowGroups
			}
			if len(declared.SelfIDs) > 0 {
				selfIDs = declared.SelfIDs
			}
		}
		views = append(views, View{
			ID:         ws.ID,
			Name:       displayName(ws.ID, conf, ws.nickname, ws.PlatformID),
			PlatformID: ws.PlatformID,
			SelfIDs:    append([]string(nil), selfIDs...),
			Groups:     groups,
			Store:      ws.Cache,
		})
	}
	return views
}

// ViewByID 返回单个工作空间视图。
func (m *Manager) ViewByID(id string) (View, bool) {
	for _, view := range m.Views() {
		if view.ID == id {
			return view, true
		}
	}
	return View{}, false
}

func displayName(id string, cfg *config.Config, nickname, platformID string) string {
	if conf, ok := cfg.WorkspaceByID(id); ok {
		if conf.Name != "" {
			return conf.Name
		}
		if conf.Nickname != "" {
			return conf.Nickname
		}
	}
	if nickname != "" {
		return nickname
	}
	if platformID != "" {
		return platformID
	}
	return id
}

// PromptPaths 返回全部工作空间需要监听的提示词文件。
func (m *Manager) PromptPaths() []string {
	paths := make([]string, 0, len(m.list)*2)
	for _, ws := range m.list {
		paths = append(paths, ws.PromptSystemPath(), ws.PromptCustomPath())
	}
	return paths
}

// PromptSystemPath 返回该工作空间实际使用的系统提示词文件。
func (w *Workspace) PromptSystemPath() string { return w.systemPath }

// PromptCustomPath 返回该工作空间的自定义提示词文件。
func (w *Workspace) PromptCustomPath() string { return w.customPath }

// ReloadPrompts 重读全部工作空间提示词并重建各自路由表。
func (m *Manager) ReloadPrompts() {
	for _, ws := range m.list {
		ws.Prompt.Reload()
		ws.Router.Rebuild()
	}
	logutil.Info("提示词已热更新并重建路由表", "工作空间数", len(m.list))
}

// Wait 等待全部工作空间的异步任务退出。
func (m *Manager) Wait() error {
	var firstErr error
	for _, ws := range m.list {
		if err := ws.Router.Wait(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ResetAll 清空全部工作空间缓存（仅测试/调试）。
func (m *Manager) ResetAll() {
	for _, ws := range m.list {
		ws.Cache.ResetAll()
	}
}

// LogSummary 打启动摘要：每个工作空间一行，方便核对 platform_id 是否配错。
func (m *Manager) LogSummary() {
	for _, ws := range m.list {
		logutil.Info("工作空间已加载",
			"id", ws.ID,
			"platform_id", ws.PlatformID,
			"self_ids", strings.Join(ws.SelfIDs, ","),
			"nickname", ws.Nickname(),
			"prompt_dir", ws.PromptDir,
			"groups", strings.Join(ws.Snapshot()().AllowGroups, ","))
	}
}
