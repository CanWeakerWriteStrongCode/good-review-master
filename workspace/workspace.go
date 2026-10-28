// Package workspace 管理机器人账号的工作空间。
//
// 一个工作空间 = 一个接收账号的可观察状态：消息缓存、选窗锚点、群人格、成员显示名。
// 档案统一落在共享 state.db，用 workspace_id 隔离；提示词是全局一份，所有工作空间共享；
// 大模型、MCP 等外部资源由组合根共享。
//
// 工作空间的键是 `{platform}_{account}`：platform 来自事件标准字段 self.platform，
// account 来自入站路径 /im/event/{account}。self.user_id 不参与归属——QQ 官方的
// 群内 openid 换群就变，做路由键会让同一账号被拆成多个工作空间。
package workspace

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/router"
	"good-review-master/telemetry"
	"good-review-master/workspace/state"
)

// platformPattern 是 OneBot 12 对平台名的约束（不允许下划线，所以不能用 qq_official）。
var platformPattern = regexp.MustCompile(`^[a-z][-a-z0-9]*(\.[-a-z0-9]+)*$`)

// idPattern 约束工作空间 ID：它会进数据库列与面板 URL。
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// WorkspaceID 把平台名与接收账号拼成工作空间 ID。
//
// 平台名里的点号替换成下划线（`zulip.my-instance` → `zulip_my-instance`），
// 因为点号在 ID 里没有额外语义；ID 会进面板 URL 和数据库列，保持字符集简单。
func WorkspaceID(platform, account string) (string, error) {
	platform = strings.ToLower(strings.TrimSpace(platform))
	account = strings.TrimSpace(account)
	if !platformPattern.MatchString(platform) {
		return "", fmt.Errorf("平台名 %q 不合法：必须匹配 [a-z][-a-z0-9]*", platform)
	}
	if !idPattern.MatchString(account) {
		return "", fmt.Errorf("账号 %q 不合法：只能包含字母、数字、下划线和短横线（最多 64 字符）", account)
	}
	id := strings.ReplaceAll(platform, ".", "_") + "_" + account
	if !idPattern.MatchString(id) {
		return "", fmt.Errorf("工作空间 ID %q 超过 64 字符", id)
	}
	return id, nil
}

// splitID 从 ID 反推平台与账号（平台名不能含下划线，所以第一个下划线是分隔符）。
func splitID(id string) (platform, account string, ok bool) {
	platform, account, ok = strings.Cut(id, "_")
	if !ok || platform == "" || account == "" {
		return "", "", false
	}
	return platform, account, true
}

// Workspace 一个接收账号的运行时。
type Workspace struct {
	ID       string
	Platform string
	Account  string

	Router *router.Router
	// Prompt 是所有工作空间共享的同一份提示词配置（全局 prompt_system.yaml +
	// prompt_custom.yaml）。私有数据一律进 state.db，不再有工作空间级配置文件。
	Prompt *config.PromptConfig
	Cache  *cache.Store
	State  *state.Scope // nil = 持久化不可用，纯内存运行

	appCfg config.Snapshot

	ob      *onebot.Client
	members *directory

	nameMu sync.RWMutex
	name   string // 面板展示名（get_self_info 的 user_name，或 meta 缓存）

	identityMu sync.RWMutex
	identities map[string]string // group_id -> 本群学到的 self.user_id（避免每条消息都写库）

	groupMu sync.RWMutex
	groups  map[string]string // group_id -> 群名；空串表示已建档但平台没给名字
}

// View 是面板用的只读视图（不含 Router/Prompt 等运行时对象）。
type View struct {
	ID       string
	Name     string
	Platform string
	Account  string
	Groups   []string
	Store    *cache.Store
	HasState bool
}

// Manager 持有全部工作空间，并负责按事件自动开通。
type Manager struct {
	appCfg      config.Snapshot
	llmClient   llm.Client
	obClient    *onebot.Client
	mcp         router.MCPProvider
	shutdownCtx context.Context
	stateStore  *state.Store
	prompt      *config.PromptConfig

	mu   sync.RWMutex
	list []*Workspace
	byID map[string]*Workspace
}

// New 从 state.db 恢复已有工作空间，之后的账号由第一条事件自动开通。
func New(appCfg config.Snapshot, systemPromptPath, customPromptPath string,
	llmClient llm.Client, obClient *onebot.Client, mcp router.MCPProvider,
	shutdownCtx context.Context) (*Manager, error) {

	// 提示词只有一份：所有工作空间共享根目录的 prompt_system.yaml / prompt_custom.yaml。
	// 分开建对象会让各 Router 的写操作各有一把锁，最终仍写同一个文件，迟早互相覆盖。
	promptCfg, err := config.LoadPromptConfig(systemPromptPath, customPromptPath)
	if err != nil {
		return nil, err
	}

	m := &Manager{
		appCfg:      appCfg,
		llmClient:   llmClient,
		obClient:    obClient,
		mcp:         mcp,
		shutdownCtx: shutdownCtx,
		prompt:      promptCfg,
		byID:        make(map[string]*Workspace),
	}
	// 所有工作空间共用一个 state.db（同表、按 workspace_id 隔离）。
	// 打不开就整体降级为纯内存，不拦住启动。
	statePath := filepath.Join(filepath.Dir(systemPromptPath), state.FileName)
	if opened, err := state.Open(statePath); err != nil {
		logutil.Error("打开 state.db 失败，全部工作空间降级为纯内存", "path", statePath, "err", err)
	} else {
		m.stateStore = opened
		opened.Report()
	}

	if err := m.restoreExisting(); err != nil {
		return nil, err
	}
	return m, nil
}

// restoreExisting 从 state.db 恢复已记录的工作空间。
//
// 工作空间没有自己的目录或配置文件，meta 表是它在磁盘上的身份；
// 所以重启直接按库恢复，不依赖第一次事件到达，面板启动即可看到。
// 名单只管新建，不限制已有工作空间的使用。
func (m *Manager) restoreExisting() error {
	if m.stateStore == nil {
		return nil
	}
	metas, err := m.stateStore.Workspaces()
	if err != nil {
		return fmt.Errorf("读取工作空间档案失败：%w", err)
	}
	for _, meta := range metas {
		if _, _, ok := splitID(meta.ID); !ok || !idPattern.MatchString(meta.ID) {
			logutil.Warn("跳过不是工作空间 ID 的档案", "workspace", meta.ID)
			continue
		}
		if meta.Platform == "" || meta.Account == "" {
			logutil.Warn("跳过缺少平台或账号的档案", "workspace", meta.ID,
				"platform", meta.Platform, "account", meta.Account)
			continue
		}
		ws, err := m.load(meta.ID, meta.Platform, meta.Account)
		if err != nil {
			logutil.Error("加载工作空间失败，跳过该档案", "id", meta.ID, "err", err)
			continue
		}
		m.list = append(m.list, ws)
		m.byID[meta.ID] = ws
		telemetry.WorkspaceResolveTotal.WithLabelValues("loaded").Inc()
	}
	sort.Slice(m.list, func(i, j int) bool { return m.list[i].ID < m.list[j].ID })
	return nil
}

// ResolveOrCreate 按 (platform, account) 找到或开通工作空间。
//
// 返回 nil 且无错误 = Go 不接管：不在 bot.allow_workspace_accounts 名单里。
// 已有工作空间由 state.db 在启动时恢复，不受名单影响；入站层据 nil 回 404，
// 让实现端把消息交回自己的 Agent。
//
// 同步部分只做本地 IO（开 SQLite、建 Router）；昵称与成员名是网络调用，
// 异步补，绝不阻塞入站。
func (m *Manager) ResolveOrCreate(platform, account string) (*Workspace, error) {
	id, err := WorkspaceID(platform, account)
	if err != nil {
		telemetry.WorkspaceResolveTotal.WithLabelValues("invalid").Inc()
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if ws := m.byID[id]; ws != nil {
		return ws, nil
	}
	if !m.appCfg().HasWorkspaceAccount(id) {
		telemetry.WorkspaceResolveTotal.WithLabelValues("denied").Inc()
		return nil, nil
	}

	ws, err := m.load(id, platform, account)
	if err != nil {
		return nil, err
	}
	logutil.Info("已自动开通工作空间", "id", id, "platform", platform, "account", account)
	telemetry.WorkspaceResolveTotal.WithLabelValues("created").Inc()
	m.list = append(m.list, ws)
	m.byID[id] = ws
	return ws, nil
}

// load 构造一个工作空间并登记到 state.db 的 meta（platform/account）。
func (m *Manager) load(id, platform, account string) (*Workspace, error) {
	cfg := m.appCfg()

	st := m.stateStore.Scope(id)
	// 平台名里的点号在 ID 中被替换成下划线，从 ID 反推会丢信息；
	// meta 里存的是原始 (platform, account)，已有记录以它为准。
	if saved := st.Meta("platform"); saved != "" {
		platform = saved
	}
	if saved := st.Meta("account"); saved != "" {
		account = saved
	}

	ws := &Workspace{
		ID:         id,
		Platform:   platform,
		Account:    account,
		Prompt:     m.prompt,
		Cache:      cache.NewStore(),
		State:      st,
		ob:         m.obClient.For(id),
		identities: make(map[string]string),
		groups:     make(map[string]string),
	}
	ws.members = newDirectory(ws.ob, st)
	ws.appCfg = func() *config.Config { return ws.snapshot(m.appCfg()) }

	ws.Router = router.NewRouter(ws.appCfg, m.prompt, m.llmClient, ws.ob, m.mcp, m.shutdownCtx)
	ws.Router.SetStore(ws.Cache)
	ws.Router.SetWorkspace(id)
	if st != nil {
		ws.Router.SetPersonaStore(st)
	}

	ws.restore(cfg)
	st.SetMeta("platform", platform)
	st.SetMeta("account", account)
	ws.scheduleNameLookup(st)
	return ws, nil
}

// restore 从 state.db 把档案读回内存：消息（尾部 N 条）、锚点、成员名、身份、群人格。
// 任何一步失败都只告警——档案是附加能力，不能拦住启动。
func (w *Workspace) restore(cfg *config.Config) {
	if w.State == nil {
		return
	}
	if msgs, err := w.State.LoadRecent(cfg.MaxCacheMsg); err != nil {
		logutil.Warn("读取历史消息失败", "workspace", w.ID, "err", err)
	} else {
		for _, msg := range msgs {
			w.Cache.GetGroupCache(msg.GroupID, cfg.MaxCacheMsg).Add(msg)
		}
		for _, groupID := range w.Cache.ListGroupIDs() {
			if gc := w.Cache.GetCache(groupID); gc != nil {
				telemetry.CacheMessages.WithLabelValues(w.ID, groupID).Set(float64(gc.Len()))
			}
		}
		logutil.Info("已从 state.db 恢复消息", "workspace", w.ID, "messages", len(msgs))
	}
	if anchors, err := w.State.LoadAnchors(); err != nil {
		logutil.Warn("读取选窗锚点失败", "workspace", w.ID, "err", err)
	} else {
		for group, anchor := range anchors {
			w.Cache.SetLLMAnchor(group, anchor)
		}
	}
	if identities, err := w.State.LoadIdentities(); err != nil {
		logutil.Warn("读取群内机器人身份失败", "workspace", w.ID, "err", err)
	} else {
		w.identityMu.Lock()
		for groupID, selfID := range identities {
			w.identities[groupID] = selfID
		}
		w.identityMu.Unlock()
	}
	if members, err := w.State.LoadMembers(); err != nil {
		logutil.Warn("读取成员名失败", "workspace", w.ID, "err", err)
	} else {
		w.members.seed(members)
	}
	if groups, err := w.State.LoadGroups(); err != nil {
		logutil.Warn("读取群档案失败", "workspace", w.ID, "err", err)
	} else {
		w.groupMu.Lock()
		for groupID, name := range groups {
			w.groups[groupID] = name
		}
		w.groupMu.Unlock()
	}
	if personas, err := w.State.LoadGroupPersonas(); err != nil {
		logutil.Warn("读取群人格失败", "workspace", w.ID, "err", err)
	} else {
		w.Router.RestorePersonas(personas)
	}
	if name := w.State.Meta("name"); name != "" {
		w.setName(name)
	}
}

// scheduleNameLookup 异步补昵称：网络调用不能挡在入站路径上。
func (w *Workspace) scheduleNameLookup(st *state.Scope) {
	if w.name != "" || w.ob == nil || w.ob.Base() == "" {
		return
	}
	go func() {
		info, err := w.ob.GetSelfInfo()
		if err != nil {
			logutil.Debug("查询机器人自身信息失败（昵称暂缺）", "workspace", w.ID, "err", err)
			return
		}
		name := info.UserDisplayName
		if name == "" {
			name = info.UserName
		}
		if name == "" {
			return
		}
		w.setName(name)
		if st != nil {
			st.SetMeta("name", name)
		}
		logutil.Info("已补全工作空间昵称", "workspace", w.ID, "name", name)
	}()
}

// Snapshot 返回带上本工作空间昵称的配置快照。
// 每次调用都从最新全局快照取，因此全局白名单的修改可以热生效。
func (w *Workspace) Snapshot() config.Snapshot { return w.appCfg }

func (w *Workspace) snapshot(global *config.Config) *config.Config {
	cp := *global
	if name := w.Name(); name != "" {
		cp.BotNickname = name
	}
	return &cp
}

// Name 返回展示名（昵称）。
func (w *Workspace) Name() string {
	w.nameMu.RLock()
	defer w.nameMu.RUnlock()
	return w.name
}

func (w *Workspace) setName(name string) {
	w.nameMu.Lock()
	w.name = name
	w.nameMu.Unlock()
}

// Nickname 返回当前生效的昵称（与 Name 相同，保留给既有调用方）。
func (w *Workspace) Nickname() string { return w.Name() }

// LearnIdentity 记住本群可比较的机器人 user_id（QQ 官方是群作用域 openid）。
// 值没变化时不写库：这个函数每条消息都会调用。
func (w *Workspace) LearnIdentity(groupID, selfUserID string) {
	if groupID == "" || selfUserID == "" {
		return
	}
	w.identityMu.Lock()
	if w.identities[groupID] == selfUserID {
		w.identityMu.Unlock()
		return
	}
	w.identities[groupID] = selfUserID
	w.identityMu.Unlock()
	w.State.SaveIdentity(groupID, selfUserID)
}

// DisplayNameFor 返回某群里某个用户的显示名（用于把 mention 渲染成 @昵称(ID)）。
// 未知成员返回空串并触发一次异步查询；渲染层会回退成「用户」。
func (w *Workspace) DisplayNameFor(groupID string) func(userID string) string {
	return func(userID string) string { return w.members.name(groupID, userID) }
}

// EnsureGroup 保证这个群在 state.db 里有档案；每个群只写一次。
//
// 群名在 QQ 官方通常拿不到，所以先落一条空名记录，再异步向实现端补；
// 补到后更新同一行。失败不影响消息处理。
func (w *Workspace) EnsureGroup(groupID string) {
	if groupID == "" {
		return
	}
	w.groupMu.Lock()
	if _, known := w.groups[groupID]; known {
		w.groupMu.Unlock()
		return
	}
	w.groups[groupID] = ""
	w.groupMu.Unlock()

	if w.State != nil {
		w.State.SaveGroup(groupID, "")
	}
	w.lookupGroupName(groupID)
}

func (w *Workspace) lookupGroupName(groupID string) {
	if w.ob == nil || w.ob.Base() == "" {
		return
	}
	go func() {
		info, err := w.ob.GetGroupInfo(groupID)
		if err != nil {
			logutil.Debug("查询群信息失败（群名暂缺）", "workspace", w.ID, "group", groupID, "err", err)
			return
		}
		name := strings.TrimSpace(info.GroupName)
		// 实现端拿不到名字时会把 group_id 原样返回，那不是群名。
		if name == "" || name == groupID {
			return
		}
		w.groupMu.Lock()
		w.groups[groupID] = name
		w.groupMu.Unlock()
		if w.State != nil {
			w.State.SaveGroup(groupID, name)
		}
	}()
}

// GroupName 返回已记录的群名；没有名字（或还没查到）返回空串。
func (w *Workspace) GroupName(groupID string) string {
	w.groupMu.RLock()
	defer w.groupMu.RUnlock()
	return w.groups[groupID]
}

// ObserveMember 记录消息里出现过的成员（发送者或 @ 对象）。
//
// QQ 官方只给 openid、不给昵称，行里的 name 可能一直为空；但它仍然是
// “已收录的人”，不能因为没有名字就丢掉。
func (w *Workspace) ObserveMember(groupID, userID string) {
	if !w.members.observe(groupID, userID) {
		return
	}
	if w.State != nil {
		w.State.SaveMember(groupID, userID, "", "")
	}
}

// MemberCount 返回已收录的群成员数（说过话或被 @ 过的人）。
// QQ 官方没有成员列表接口，所以这是观察到的数量，不是群真实人数。
func (w *Workspace) MemberCount(groupID string) int {
	return w.members.count(groupID)
}

// AllowsGroup 判断群是否在白名单内。
func (w *Workspace) AllowsGroup(groupID string) bool {
	for _, g := range w.Snapshot()().AllowGroups {
		if g == "*" || g == groupID {
			return true
		}
	}
	return false
}

// AllowsAllGroups 判断是否放行全部群。
func (w *Workspace) AllowsAllGroups() bool {
	for _, g := range w.Snapshot()().AllowGroups {
		if g == "*" {
			return true
		}
	}
	return false
}

// snapshot 复制一份工作空间列表：自动开通会并发 append，读者不能直接遍历底层切片。
func (m *Manager) snapshot() []*Workspace {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]*Workspace(nil), m.list...)
}

// List 返回全部工作空间（按 ID 排序的副本）。
func (m *Manager) List() []*Workspace { return m.snapshot() }

// Default 只在一个工作空间时返回它。多工作空间下不猜账号：调用方必须给 workspace_id。
func (m *Manager) Default() *Workspace {
	list := m.snapshot()
	if len(list) == 1 {
		return list[0]
	}
	return nil
}

// ByID 按 ID 查找工作空间。
func (m *Manager) ByID(id string) (*Workspace, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ws, ok := m.byID[id]
	return ws, ok
}

// NicknameUnique 判断该昵称是否唯一属于一个工作空间。
// 只有唯一时才能用 "@昵称" 做归属兜底：同名账号下无法判断 @ 的是谁。
func (m *Manager) NicknameUnique(nickname string) bool {
	if nickname == "" {
		return false
	}
	hits := 0
	for _, ws := range m.snapshot() {
		if ws.Name() == nickname {
			hits++
		}
	}
	return hits == 1
}

// Views 返回面板用的工作空间视图。
func (m *Manager) Views() []View {
	list := m.snapshot()
	views := make([]View, 0, len(list))
	for _, ws := range list {
		views = append(views, View{
			ID:       ws.ID,
			Name:     displayName(ws),
			Platform: ws.Platform,
			Account:  ws.Account,
			Groups:   append([]string(nil), ws.Snapshot()().AllowGroups...),
			Store:    ws.Cache,
			HasState: ws.State != nil,
		})
	}
	return views
}

// GroupName 返回某个工作空间下已记录的群名（空串表示平台没给）。
func (m *Manager) GroupName(workspaceID, groupID string) string {
	ws := m.get(workspaceID)
	if ws == nil {
		return ""
	}
	return ws.GroupName(groupID)
}

// MemberCount 返回某个工作空间下已收录的群成员数。
func (m *Manager) MemberCount(workspaceID, groupID string) int {
	ws := m.get(workspaceID)
	if ws == nil {
		return 0
	}
	return ws.MemberCount(groupID)
}

func (m *Manager) get(workspaceID string) *Workspace {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.byID[workspaceID]
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

func displayName(ws *Workspace) string {
	if name := ws.Name(); name != "" {
		return name
	}
	return ws.ID
}

// ReloadPrompts 重读共享提示词并重建每个工作空间的路由表。
// 数据只有一份，但要逐个重建 Router：路由表里存着按人格展开的指令。
func (m *Manager) ReloadPrompts() {
	m.prompt.Reload()
	list := m.snapshot()
	for _, ws := range list {
		ws.Router.Rebuild()
	}
	logutil.Info("提示词已热更新并重建路由表", "工作空间数", len(list))
}

// Wait 等待全部工作空间的异步任务退出。
func (m *Manager) Wait() error {
	var firstErr error
	for _, ws := range m.snapshot() {
		if err := ws.Router.Wait(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Close 关闭共享的 state.db。必须在 Wait 之后调用。
func (m *Manager) Close() {
	if err := m.stateStore.Close(); err != nil {
		logutil.Warn("关闭 state.db 失败", "err", err)
	}
}

// ResetAll 清空全部工作空间缓存（仅测试/调试）。
func (m *Manager) ResetAll() {
	for _, ws := range m.snapshot() {
		ws.Cache.ResetAll()
	}
}

// LogSummary 打启动摘要。
func (m *Manager) LogSummary() {
	for _, ws := range m.snapshot() {
		logutil.Info("工作空间已加载",
			"id", ws.ID,
			"platform", ws.Platform,
			"account", ws.Account,
			"nickname", ws.Name(),
			"persistent", ws.State != nil,
			"groups", strings.Join(ws.Snapshot()().AllowGroups, ","))
	}
}

// directory 群成员显示名目录：内存优先，未命中时异步查 get_group_member_info。
type directory struct {
	mu    sync.RWMutex
	names map[string]map[string]string
	// seen 记录“出现过”的成员（有名字或只有 ID），用于统计已收录人数；
	// names 只负责展示名，两者不能混用。
	seen    map[string]map[string]struct{}
	pending map[string]map[string]struct{}
	ob      *onebot.Client
	state   *state.Scope
}

func newDirectory(ob *onebot.Client, st *state.Scope) *directory {
	return &directory{
		names:   make(map[string]map[string]string),
		seen:    make(map[string]map[string]struct{}),
		pending: make(map[string]map[string]struct{}),
		ob:      ob,
		state:   st,
	}
}

func (d *directory) seed(all map[string]map[string]string) {
	if len(all) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for group, members := range all {
		if d.names[group] == nil {
			d.names[group] = make(map[string]string, len(members))
		}
		if d.seen[group] == nil {
			d.seen[group] = make(map[string]struct{}, len(members))
		}
		for userID, name := range members {
			d.names[group][userID] = name
			d.seen[group][userID] = struct{}{}
		}
	}
}

// observe 记录一个出现过的成员，返回是否首次见到。
func (d *directory) observe(groupID, userID string) bool {
	if groupID == "" || userID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.seen[groupID] == nil {
		d.seen[groupID] = make(map[string]struct{})
	}
	if _, ok := d.seen[groupID][userID]; ok {
		return false
	}
	d.seen[groupID][userID] = struct{}{}
	return true
}

// count 返回该群已收录的成员数（说过话或被 @ 过的人，不要求有名字）。
func (d *directory) count(groupID string) int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.seen[groupID])
}

func (d *directory) name(groupID, userID string) string {
	if userID == "" {
		return ""
	}
	d.mu.RLock()
	name := d.names[groupID][userID]
	d.mu.RUnlock()
	if name != "" {
		return name
	}
	d.fetchAsync(groupID, userID)
	return ""
}

func (d *directory) fetchAsync(groupID, userID string) {
	if d.ob == nil || d.ob.Base() == "" {
		return
	}
	d.mu.Lock()
	if d.pending[groupID] == nil {
		d.pending[groupID] = make(map[string]struct{})
	}
	if _, ok := d.pending[groupID][userID]; ok {
		d.mu.Unlock()
		return
	}
	d.pending[groupID][userID] = struct{}{}
	d.mu.Unlock()

	go func() {
		defer func() {
			d.mu.Lock()
			delete(d.pending[groupID], userID)
			d.mu.Unlock()
		}()
		info, err := d.ob.GetGroupMemberInfo(groupID, userID)
		if err != nil {
			return
		}
		name := info.DisplayName()
		if name == "" {
			return
		}
		d.mu.Lock()
		if d.names[groupID] == nil {
			d.names[groupID] = make(map[string]string)
		}
		if d.seen[groupID] == nil {
			d.seen[groupID] = make(map[string]struct{})
		}
		d.names[groupID][userID] = name
		d.seen[groupID][userID] = struct{}{}
		d.mu.Unlock()
		if d.state != nil {
			d.state.SaveMember(groupID, userID, info.UserName, info.UserDisplayName)
		}
	}()
}
