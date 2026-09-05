// Package mcpclient 管理 MCP 工具服务：自动连接、自动拉取工具清单并做稳定快照、代理工具调用。
//
// 设计要点：
//   - 工具清单以不可变快照（atomic.Pointer）整体替换，对话期读取完全无锁；
//   - 快照内工具按暴露名字典序排序，保证下发给大模型的 tools 字段字节稳定，
//     不击穿 prefix cache —— 本项目按缓存命中价做扩展/重置决策，前缀抖动会直接抬成本；
//   - 连接失败不阻塞启动，后台按 retry_interval 重连；服务端推 tools/list_changed
//     通知时自动重拉清单，无需重启。
package mcpclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/version"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// implName initialize 握手里报给对端的客户端标识
const implName = "good-review"

// toolJSONOverheadTokens 每个工具在 OpenAI tools 字段里的 JSON 骨架开销折算 token 数
// （{"type":"function","function":{"name":"","description":"","parameters":}}）
const toolJSONOverheadTokens = 10

// maxListPages tools/list 分页保护上限，防止服务端返回固定游标导致死循环
const maxListPages = 50

// Manager MCP 工具服务管理器。零值不可用，必须经 New 构造。
type Manager struct {
	cfg      config.MCPConf
	lifetime context.Context // 会话生命周期，随主程序 shutdownCtx 一起取消
	cancel   context.CancelFunc

	mu      sync.Mutex // 只保护快照重建的串行化（读侧走 snap，无锁）
	servers []*serverConn
	snap    atomic.Pointer[snapshot]

	closeOnce sync.Once
}

// serverConn 单个 MCP 服务的连接状态
type serverConn struct {
	cfg config.MCPServerConf
	mgr *Manager

	mu      sync.Mutex
	session *mcp.ClientSession // nil 表示当前未连接
	tools   []*mcp.Tool        // 最近一次成功拉到的工具清单
	lastErr error
}

// binding 暴露给模型的工具名 → 归属服务与 MCP 原始工具名
type binding struct {
	server *serverConn
	tool   string
}

// snapshot 不可变工具清单快照。发布后只读，整体替换，读侧无锁。
type snapshot struct {
	tools    []llm.Tool          // 注入对话用的工具清单（只含 inject 服务、只含在线服务），按 Name 排序
	bindings map[string]*binding // 暴露名 → 调用目标
	tokens   int                 // 清单折算的 token 数，计入缓存成本模型的固定前缀
}

// ServerStatus 单个服务的运行状态（日志与 Web 面板展示用）
type ServerStatus struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	Inject    bool     `json:"inject"`
	Online    bool     `json:"online"`
	Tools     []string `json:"tools"`
	Error     string   `json:"error,omitempty"`
}

// New 构造管理器。enabled=false 或没有可用服务时返回一个空转的 Manager
// （Tools 恒为空、CallTool 恒报无工具），调用方无需判空。
func New(cfg config.MCPConf, shutdownCtx context.Context) *Manager {
	lifetime, cancel := context.WithCancel(shutdownCtx)
	m := &Manager{cfg: cfg, lifetime: lifetime, cancel: cancel}
	if !cfg.Enabled {
		return m
	}
	for i := range cfg.Servers {
		m.servers = append(m.servers, &serverConn{cfg: cfg.Servers[i], mgr: m})
	}
	m.snap.Store(&snapshot{bindings: map[string]*binding{}})
	return m
}

// LogConfig 在初始化阶段把 MCP 配置里的所有服务逐条以 Info 打印，供启动时核对加载结果。
// 只打安全信息：http/sse 端点的 query 参数值一律掩成 ***，token 只报有无，env 只打 key 名不打值。
func (m *Manager) LogConfig() {
	if !m.cfg.Enabled {
		logutil.Info("MCP 功能未启用（config mcp.enabled=false），对话不会注入工具")
		return
	}
	logutil.Info("MCP 配置加载", "服务数", len(m.servers),
		"工具超时", m.cfg.ToolTimeout.String(),
		"最大调用轮数", m.cfg.MaxToolRounds,
		"结果截断字符", m.cfg.MaxToolResultRune,
		"重连间隔", m.cfg.RetryInterval.String())
	for _, s := range m.servers {
		if s.cfg.Transport == "http" || s.cfg.Transport == "sse" {
			token := "未配置"
			if strings.TrimSpace(s.cfg.Token) != "" {
				token = "已配置(Bearer，值隐藏)"
			}
			logutil.Info("MCP 服务配置", "name", s.cfg.Name, "transport", s.cfg.Transport,
				"inject", s.cfg.ShouldInject(), "endpoint", maskURLParams(s.cfg.URL), "token", token)
			continue
		}
		// stdio：命令 + 参数原样，env 只列 key 名
		logutil.Info("MCP 服务配置", "name", s.cfg.Name, "transport", "stdio",
			"inject", s.cfg.ShouldInject(), "command", strings.TrimSpace(s.cfg.Command),
			"args", s.cfg.Args, "env_keys", sortedMapKeys(s.cfg.Env))
	}
}

// maskURLParams 隐藏 URL query 的参数值（保留参数名与结构），避免把 key/token 打进日志
func maskURLParams(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "(url 解析失败，已隐藏)"
	}
	if u.RawQuery == "" {
		return u.Scheme + "://" + u.Host + u.Path
	}
	q := u.Query()
	names := make([]string, 0, len(q))
	for k := range q {
		names = append(names, k)
	}
	sort.Strings(names)
	var b strings.Builder
	for i, k := range names {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteString("=***")
	}
	return u.Scheme + "://" + u.Host + u.Path + "?" + b.String()
}

// sortedMapKeys 返回 map 的 key 名（排序），只用于日志展示 env 配置项
func sortedMapKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Start 后台并发连接所有服务并拉取工具清单。不阻塞调用方：
// 连不上的服务留待重连循环处理，连上的会立刻原子更新快照开始生效。
func (m *Manager) Start() {
	if len(m.servers) == 0 {
		return
	}
	logutil.Info("MCP 工具服务连接中（后台进行，完成后自动注入对话）", "数量", len(m.servers))
	go func() {
		var wg sync.WaitGroup
		for _, s := range m.servers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.dial()
			}()
		}
		wg.Wait()
		m.logSummary()
		go m.reconnectLoop()
	}()
}

// Tools 返回当前可注入对话的工具清单快照（按名排序、发布后不再变动）。
// 返回的切片由调用方只读使用，不得修改。
func (m *Manager) Tools() []llm.Tool {
	if snap := m.snap.Load(); snap != nil {
		return snap.tools
	}
	return nil
}

// ToolsTokens 返回工具清单折算的 token 数。
// 它会随 system prompt 一起构成"每次请求都必然发送的固定前缀"，
// 必须计入缓存扩展/重置的成本决策，否则会低估上下文长度、撞穿 max_context_tokens 护栏。
func (m *Manager) ToolsTokens() int {
	if snap := m.snap.Load(); snap != nil {
		return snap.tokens
	}
	return 0
}

// Status 返回所有服务的连接状态（含失败原因与已获取的工具名）
func (m *Manager) Status() []ServerStatus {
	out := make([]ServerStatus, 0, len(m.servers))
	for _, s := range m.servers {
		s.mu.Lock()
		st := ServerStatus{
			Name:      s.cfg.Name,
			Transport: s.cfg.Transport,
			Inject:    s.cfg.ShouldInject(),
			Online:    s.session != nil,
		}
		for _, t := range s.tools {
			st.Tools = append(st.Tools, t.Name)
		}
		if s.lastErr != nil {
			st.Error = s.lastErr.Error()
		}
		s.mu.Unlock()
		out = append(out, st)
	}
	return out
}

// CallTool 按暴露名调用工具：解析模型给的 JSON 入参 → 路由到归属服务 → tools/call → 拍平结果文本。
// 结果文本已由调用方负责截断；这里只在服务端明确报错时返回 error（错误文本同时带出，便于回填给模型自纠）。
func (m *Manager) CallTool(ctx context.Context, exposedName, argsJSON string) (string, error) {
	snap := m.snap.Load()
	if snap == nil {
		return "", errors.New("MCP 未启用")
	}
	b, ok := snap.bindings[exposedName]
	if !ok {
		return "", fmt.Errorf("工具不存在或已下线: %s", exposedName)
	}

	var args map[string]any
	if s := strings.TrimSpace(argsJSON); s != "" && s != "null" {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			return "", fmt.Errorf("模型给的入参不是合法 JSON 对象（%s）: %w", s, err)
		}
	}

	b.server.mu.Lock()
	session := b.server.session
	b.server.mu.Unlock()
	if session == nil {
		return "", fmt.Errorf("MCP 服务 %s 当前未连接", b.server.cfg.Name)
	}

	start := time.Now()
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: b.tool, Arguments: args})
	if err != nil {
		// 连接已断：摘掉会话让重连循环接手，同时把它的工具从快照里撤下
		if errors.Is(err, mcp.ErrConnectionClosed) {
			b.server.markDead(err)
		}
		return "", fmt.Errorf("调用 %s.%s 失败: %w", b.server.cfg.Name, b.tool, err)
	}

	text, toolErr := flattenContent(res)
	logutil.Debug("MCP 工具返回", "server", b.server.cfg.Name, "tool", b.tool,
		"耗时", time.Since(start).String(), "字符数", len([]rune(text)), "isError", toolErr)
	if toolErr {
		return text, fmt.Errorf("工具执行报错: %s", text)
	}
	return text, nil
}

// Close 关闭所有会话（stdio 子进程随之被终止）并停止重连循环
func (m *Manager) Close() error {
	var err error
	m.closeOnce.Do(func() {
		m.cancel()
		for _, s := range m.servers {
			s.mu.Lock()
			session := s.session
			s.session = nil
			s.mu.Unlock()
			if session == nil {
				continue
			}
			if closeErr := session.Close(); closeErr != nil {
				logutil.Warn("关闭 MCP 会话失败", "server", s.cfg.Name, "err", closeErr)
				if err == nil {
					err = closeErr
				}
			}
		}
	})
	return err
}

// dial 连接一个服务并拉取工具清单，成功或失败都只记日志，不向上抛
func (s *serverConn) dial() {
	ctx, cancel := context.WithTimeout(s.mgr.lifetime, s.mgr.opTimeout())
	defer cancel()

	session, tools, err := s.connect(ctx)
	s.mu.Lock()
	if err != nil {
		s.session = nil
		s.lastErr = err
	} else {
		s.session = session
		s.tools = tools
		s.lastErr = nil
	}
	s.mu.Unlock()

	if err != nil {
		logutil.Warn("MCP 服务连接失败，等待重连", "server", s.cfg.Name,
			"transport", s.cfg.Transport, "err", err)
		return
	}
	s.mgr.rebuildSnapshot()
	logutil.Info("MCP 服务已连接", "server", s.cfg.Name, "transport", s.cfg.Transport,
		"工具数", len(tools), "注入", s.cfg.ShouldInject())
	s.logFetchedTools(tools)
}

// logFetchedTools 把一个服务实际获取到的每个工具完整定义逐条以 Info 打出
// （原始名 + 描述 + 入参 schema），不只打名字。inject=false 只连不注入的服务也会打。
func (s *serverConn) logFetchedTools(tools []*mcp.Tool) {
	if len(tools) == 0 {
		logutil.Info("MCP 服务获取到工具", "server", s.cfg.Name, "工具", "(空清单)")
		return
	}
	for _, t := range tools {
		if t == nil {
			continue
		}
		logutil.Info("MCP 服务获取到工具", "server", s.cfg.Name,
			"名称", t.Name,
			"描述", strings.TrimSpace(t.Description),
			"入参schema", string(marshalInputSchema(t.InputSchema)))
	}
}

// connect 建连 + initialize 握手 + 拉全量工具清单。任一步失败都会关掉已建会话再返回错误。
func (s *serverConn) connect(ctx context.Context) (*mcp.ClientSession, []*mcp.Tool, error) {
	transport, err := s.buildTransport(ctx)
	if err != nil {
		return nil, nil, err
	}

	client := mcp.NewClient(&mcp.Implementation{Name: implName, Version: version.String()}, &mcp.ClientOptions{
		// 服务端主动通知工具清单变化 → 另起 goroutine 重拉。
		// 不能在通知回调里同步调 ListTools：回调跑在连接的读循环上，会自锁。
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			go s.refreshTools()
		},
	})

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("握手失败: %w", err)
	}
	tools, err := listAllTools(ctx, session)
	if err != nil {
		_ = session.Close()
		return nil, nil, fmt.Errorf("拉取工具清单失败: %w", err)
	}
	return session, tools, nil
}

// refreshTools 在已建会话上重拉工具清单并刷新快照（tools/list_changed 通知触发）
func (s *serverConn) refreshTools() {
	s.mu.Lock()
	session := s.session
	s.mu.Unlock()
	if session == nil {
		return
	}

	ctx, cancel := context.WithTimeout(s.mgr.lifetime, s.mgr.opTimeout())
	defer cancel()
	tools, err := listAllTools(ctx, session)
	if err != nil {
		logutil.Warn("MCP 工具清单刷新失败", "server", s.cfg.Name, "err", err)
		return
	}
	s.mu.Lock()
	s.tools = tools
	s.mu.Unlock()
	s.mgr.rebuildSnapshot()
	logutil.Info("MCP 工具清单已刷新", "server", s.cfg.Name, "工具数", len(tools))
}

// markDead 摘掉已断开的会话，让重连循环接手，并立刻把它贡献的工具从快照撤下。
// 撤下后模型不会再看到这些工具，避免持续调用一个已死的服务。
func (s *serverConn) markDead(cause error) {
	s.mu.Lock()
	session := s.session
	s.session = nil
	s.lastErr = cause
	s.mu.Unlock()
	if session != nil {
		_ = session.Close()
	}
	logutil.Warn("MCP 会话已断开，等待重连", "server", s.cfg.Name, "err", cause)
	s.mgr.rebuildSnapshot()
}

// reconnectLoop 定期重连掉线的服务；retry_interval_sec <= 0 时不启动
func (m *Manager) reconnectLoop() {
	if m.cfg.RetryInterval <= 0 {
		logutil.Info("MCP 自动重连已禁用", "server数", len(m.servers))
		return
	}
	ticker := time.NewTicker(m.cfg.RetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.lifetime.Done():
			return
		case <-ticker.C:
			for _, s := range m.servers {
				s.mu.Lock()
				dead := s.session == nil
				s.mu.Unlock()
				if dead {
					logutil.Debug("MCP 重连尝试", "server", s.cfg.Name)
					s.dial()
				}
			}
		}
	}
}

// rebuildSnapshot 汇总所有在线且 inject 的服务工具，重建并原子发布快照。
// 加 m.mu 是为了串行化重建，避免两个服务同时上线时后写的快照漏掉先写的。
func (m *Manager) rebuildSnapshot() {
	m.mu.Lock()
	defer m.mu.Unlock()

	type entry struct {
		serverName string
		rawName    string // MCP 原始工具名，调用时用它
		exposed    string // 规整后的候选暴露名
		desc       string
		params     json.RawMessage
		bind       *binding
	}
	var entries []entry
	nameCount := make(map[string]int)

	for _, s := range m.servers {
		if !s.cfg.ShouldInject() {
			continue
		}
		s.mu.Lock()
		online := s.session != nil
		tools := s.tools
		s.mu.Unlock()
		if !online {
			continue
		}
		for _, t := range tools {
			if t == nil || t.Name == "" {
				continue
			}
			exposed := sanitizeToolName(t.Name)
			nameCount[exposed]++
			entries = append(entries, entry{
				serverName: s.cfg.Name,
				rawName:    t.Name,
				exposed:    exposed,
				desc:       strings.TrimSpace(t.Description),
				params:     marshalInputSchema(t.InputSchema),
				bind:       &binding{server: s, tool: t.Name},
			})
		}
	}

	// 先按 (服务名, 原始工具名) 稳定排序再分配暴露名：
	// 服务端返回的工具顺序不保证稳定，不排序会让同一份配置每次重建快照得到
	// 不同的名字集合，直接击穿大模型的 prefix cache（命中价变未命中价）。
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].serverName != entries[j].serverName {
			return entries[i].serverName < entries[j].serverName
		}
		return entries[i].rawName < entries[j].rawName
	})

	snap := &snapshot{bindings: make(map[string]*binding, len(entries))}
	renamed := 0
	for _, e := range entries {
		exposed := e.exposed
		// 跨服务重名：加 "<服务名>__" 前缀消歧
		if nameCount[e.exposed] > 1 {
			exposed = sanitizeToolName(e.serverName + "__" + e.exposed)
			renamed++
		}
		// 消歧后仍撞名（典型场景：工具名全是中文，被规整成空后兜底为 "tool"）
		// → 追加序号，宁可变长也不丢工具
		if _, dup := snap.bindings[exposed]; dup {
			for n := 2; ; n++ {
				candidate := sanitizeToolName(fmt.Sprintf("%s_%d", exposed, n))
				if _, stillDup := snap.bindings[candidate]; !stillDup {
					exposed = candidate
					break
				}
			}
			renamed++
		}
		snap.bindings[exposed] = e.bind
		snap.tools = append(snap.tools, llm.Tool{Name: exposed, Description: e.desc, ParamsJSON: e.params})
		snap.tokens += cache.EstimateTokens(exposed) + cache.EstimateTokens(e.desc) +
			cache.EstimateTokens(string(e.params)) + toolJSONOverheadTokens
	}
	if renamed > 0 {
		logutil.Warn("MCP 工具名有冲突，已自动加前缀/序号消歧", "涉及工具数", renamed)
	}
	sortTools(snap.tools)
	m.snap.Store(snap)
}

// logSummary 首轮连接结束后打一条汇总，并把失败原因逐个列出来
func (m *Manager) logSummary() {
	snap := m.snap.Load()
	toolCount, tokens := 0, 0
	if snap != nil {
		toolCount, tokens = len(snap.tools), snap.tokens
	}
	online := 0
	for _, s := range m.servers {
		s.mu.Lock()
		up := s.session != nil
		s.mu.Unlock()
		if up {
			online++
		}
	}
	logutil.Info("MCP 工具服务就绪", "在线", online, "总数", len(m.servers),
		"注入工具数", toolCount, "工具token估算", tokens)
	// 在线服务的完整工具定义已由各自 dial() 里的 logFetchedTools 打出，这里只报失败的服务
	for _, st := range m.Status() {
		if st.Error != "" {
			logutil.Warn("MCP 服务不可用", "server", st.Name, "transport", st.Transport, "err", st.Error)
		}
	}
	if snap != nil {
		names := make([]string, 0, len(snap.tools))
		for _, t := range snap.tools {
			names = append(names, t.Name)
		}
		logutil.Info("实际注入对话的工具清单", "工具数", len(names), "工具", names)
	}
}

// opTimeout 单次 MCP 操作（连接/列工具）的超时，复用 tool_timeout_sec
func (m *Manager) opTimeout() time.Duration {
	if m.cfg.ToolTimeout > 0 {
		return m.cfg.ToolTimeout
	}
	return 30 * time.Second
}

// listAllTools 拉全量工具清单（跟随 nextCursor 分页）
func listAllTools(ctx context.Context, session *mcp.ClientSession) ([]*mcp.Tool, error) {
	var tools []*mcp.Tool
	params := &mcp.ListToolsParams{}
	for page := 0; page < maxListPages; page++ {
		res, err := session.ListTools(ctx, params)
		if err != nil {
			return nil, err
		}
		tools = append(tools, res.Tools...)
		if res.NextCursor == "" || res.NextCursor == params.Cursor {
			return tools, nil
		}
		params.Cursor = res.NextCursor
	}
	return tools, nil
}
