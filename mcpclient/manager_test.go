package mcpclient

import (
	"context"
	"strings"
	"testing"
	"time"

	"good-review-master/config"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// newTestManager 造一个空管理器（不连任何真实服务），用于快照重建的单测
func newTestManager() *Manager {
	m := &Manager{cfg: config.MCPConf{Enabled: true, ToolTimeout: 5 * time.Second}}
	m.snap.Store(&snapshot{bindings: map[string]*binding{}})
	return m
}

// attachStubServer 挂一个假的"在线"服务：用零值会话占位，仅供快照重建类单测使用。
// 零值 ClientSession 不能真发请求（内部连接为 nil），要真调用请用 attachLiveServer。
func attachStubServer(m *Manager, name string, inject *bool, tools ...*mcp.Tool) *serverConn {
	s := &serverConn{cfg: config.MCPServerConf{Name: name, Transport: "http", Inject: inject}, mgr: m}
	s.session = &mcp.ClientSession{}
	s.tools = tools
	m.servers = append(m.servers, s)
	return s
}

// attachLiveServer 挂一个持有真实会话的在线服务，可直接发起 tools/call
func attachLiveServer(m *Manager, name string, session *mcp.ClientSession, tools ...*mcp.Tool) *serverConn {
	s := &serverConn{cfg: config.MCPServerConf{Name: name, Transport: "stdio"}, mgr: m}
	s.session = session
	s.tools = tools
	m.servers = append(m.servers, s)
	return s
}

func toolNames(m *Manager) []string {
	var names []string
	for _, t := range m.Tools() {
		names = append(names, t.Name)
	}
	return names
}

func boolPtr(v bool) *bool { return &v }

// 快照只收「在线 且 inject」的服务工具，并按名字典序排列
func TestRebuildSnapshot_筛选与排序(t *testing.T) {
	setupTestLogger(t)
	m := newTestManager()
	attachStubServer(m, "zebra", nil, &mcp.Tool{Name: "z_tool", Description: "z"})
	attachStubServer(m, "alpha", nil, &mcp.Tool{Name: "a_tool", Description: "a"})
	// inject=false：连了但不注入
	attachStubServer(m, "muted", boolPtr(false), &mcp.Tool{Name: "muted_tool"})
	// 离线：session 为 nil
	offline := &serverConn{cfg: config.MCPServerConf{Name: "down", Transport: "http"}, mgr: m}
	offline.tools = []*mcp.Tool{{Name: "down_tool"}}
	m.servers = append(m.servers, offline)

	m.rebuildSnapshot()

	got := strings.Join(toolNames(m), ",")
	if got != "a_tool,z_tool" {
		t.Fatalf("快照内容不对: %s", got)
	}
	if m.ToolsTokens() <= 0 {
		t.Fatalf("token 估算应大于 0，得到 %d", m.ToolsTokens())
	}
	// 未注入/离线的工具不能被调到
	if _, err := m.CallTool(context.Background(), "muted_tool", ""); err == nil {
		t.Fatal("inject=false 的工具不该可调用")
	}
	if _, err := m.CallTool(context.Background(), "down_tool", ""); err == nil {
		t.Fatal("离线服务的工具不该可调用")
	}
}

// 快照必须稳定：同样的输入重建多次，得到的工具名序列完全一致。
// 顺序抖动会让下发给大模型的 tools 字段字节变化，直接击穿 prefix cache。
func TestRebuildSnapshot_多次重建结果稳定(t *testing.T) {
	setupTestLogger(t)
	build := func() string {
		m := newTestManager()
		attachStubServer(m, "b", nil,
			&mcp.Tool{Name: "b2"}, &mcp.Tool{Name: "b1"}, &mcp.Tool{Name: "b3"})
		attachStubServer(m, "a", nil,
			&mcp.Tool{Name: "a2"}, &mcp.Tool{Name: "a1"})
		m.rebuildSnapshot()
		return strings.Join(toolNames(m), ",")
	}
	first := build()
	if first != "a1,a2,b1,b2,b3" {
		t.Fatalf("排序不对: %s", first)
	}
	for i := 0; i < 20; i++ {
		if got := build(); got != first {
			t.Fatalf("第 %d 次重建结果漂移: %s（首次 %s）", i, got, first)
		}
	}
}

// 跨服务重名加 "<服务名>__" 前缀；同服务内规整后仍撞名则追加序号，一个工具都不丢
func TestRebuildSnapshot_重名消歧(t *testing.T) {
	setupTestLogger(t)

	t.Run("跨服务重名加服务名前缀", func(t *testing.T) {
		m := newTestManager()
		attachStubServer(m, "amap", nil, &mcp.Tool{Name: "search"})
		attachStubServer(m, "bing", nil, &mcp.Tool{Name: "search"})
		m.rebuildSnapshot()
		if got := strings.Join(toolNames(m), ","); got != "amap__search,bing__search" {
			t.Fatalf("消歧结果不对: %s", got)
		}
		// 两个都要能路由回各自的服务
		for exposed, wantServer := range map[string]string{
			"amap__search": "amap",
			"bing__search": "bing",
		} {
			b, ok := m.snap.Load().bindings[exposed]
			if !ok || b.server.cfg.Name != wantServer {
				t.Fatalf("%s 未正确路由到 %s", exposed, wantServer)
			}
			if b.tool != "search" {
				t.Fatalf("%s 应映射回 MCP 原始名 search，得到 %s", exposed, b.tool)
			}
		}
	})

	t.Run("中文名工具规整后撞名追加序号", func(t *testing.T) {
		m := newTestManager()
		attachStubServer(m, "cn", nil,
			&mcp.Tool{Name: "查询天气"}, &mcp.Tool{Name: "查询路况"}, &mcp.Tool{Name: "查询航班"})
		m.rebuildSnapshot()
		// 三个中文名先被规整成同名（计数>1）→ 加服务名前缀 → 仍同名再追加序号，一个不丢
		if got := strings.Join(toolNames(m), ","); got != "cn__tool,cn__tool_2,cn__tool_3" {
			t.Fatalf("消歧结果不对: %s", got)
		}
		if len(m.snap.Load().bindings) != 3 {
			t.Fatal("撞名的工具被丢掉了")
		}
		// 序号分配必须确定：按 (服务名, 原始工具名) 排序后依次编号
		if b := m.snap.Load().bindings["cn__tool"]; b.tool != "查询天气" {
			t.Fatalf("cn__tool 应映射到查询天气，得到 %s", b.tool)
		}
	})
}

// 工具名要规整成 OpenAI 允许的字符集，入参 schema 要原样透传
func TestRebuildSnapshot_工具定义转换(t *testing.T) {
	setupTestLogger(t)
	m := newTestManager()
	attachStubServer(m, "amap", nil, &mcp.Tool{
		Name:        "maps.directions/driving",
		Description: "  驾车路线规划  ",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"origin": map[string]any{"type": "string"}}},
	})
	m.rebuildSnapshot()

	tools := m.Tools()
	if len(tools) != 1 {
		t.Fatalf("期望 1 个工具，得到 %d", len(tools))
	}
	if tools[0].Name != "maps_directions_driving" {
		t.Fatalf("工具名未规整: %s", tools[0].Name)
	}
	if tools[0].Description != "驾车路线规划" {
		t.Fatalf("描述未去空白: %q", tools[0].Description)
	}
	if !strings.Contains(string(tools[0].ParamsJSON), `"origin"`) {
		t.Fatalf("入参 schema 丢失: %s", tools[0].ParamsJSON)
	}
}

// MCP 未启用时管理器空转：工具清单恒空、调用直接报错，且 Start/Close 都不能炸
func TestNew_未启用时空转(t *testing.T) {
	setupTestLogger(t)
	m := New(config.MCPConf{Enabled: false}, context.Background())
	m.Start()
	defer m.Close()

	if tools := m.Tools(); tools != nil {
		t.Fatalf("未启用时工具清单应为 nil，得到 %v", tools)
	}
	if tokens := m.ToolsTokens(); tokens != 0 {
		t.Fatalf("未启用时 token 应为 0，得到 %d", tokens)
	}
	if _, err := m.CallTool(context.Background(), "any", ""); err == nil {
		t.Fatal("未启用时调用工具应报错")
	}
	if st := m.Status(); len(st) != 0 {
		t.Fatalf("未启用时状态列表应为空，得到 %v", st)
	}
}

// 配置了服务但一个都连不上：快照为空，对话侧会自动退回单轮无工具路径
func TestNew_启用但无可用服务(t *testing.T) {
	setupTestLogger(t)
	m := New(config.MCPConf{
		Enabled: true,
		Servers: []config.MCPServerConf{{Name: "unreachable", Transport: "http", URL: "http://127.0.0.1:1/mcp"}},
	}, context.Background())
	defer m.Close()

	if len(m.Tools()) != 0 {
		t.Fatal("还没连接时工具清单应为空")
	}
	st := m.Status()
	if len(st) != 1 || st[0].Online || st[0].Name != "unreachable" {
		t.Fatalf("状态不对: %+v", st)
	}
}

// weatherIn 内存 MCP 服务端的工具入参（schema 由 SDK 自动推导）
type weatherIn struct {
	City string `json:"city"`
}

// startInMemorySession 用 SDK 的内存传输起一个真实 MCP 服务端并完成握手，
// 返回客户端会话。走的是真实 JSON-RPC 协议栈，能覆盖 listAllTools / CallTool 的实际行为。
func startInMemorySession(t *testing.T) (*mcp.ClientSession, context.Context) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	srv := mcp.NewServer(&mcp.Implementation{Name: "fake-mcp", Version: "v1"}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "get_weather", Description: "查询城市天气"},
		func(_ context.Context, _ *mcp.CallToolRequest, in weatherIn) (*mcp.CallToolResult, any, error) {
			if in.City == "" {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "参数 city 不能为空"}},
					IsError: true,
				}, nil, nil
			}
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: in.City + " 晴，32℃"}},
			}, nil, nil
		})
	mcp.AddTool(srv, &mcp.Tool{Name: "查询路况", Description: "中文名工具"},
		func(_ context.Context, _ *mcp.CallToolRequest, _ weatherIn) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{
				Content: []mcp.Content{&mcp.TextContent{Text: "路况畅通"}},
			}, nil, nil
		})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("启动内存 MCP 服务端失败: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: implName, Version: "v1"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("连接内存 MCP 服务端失败: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })
	return clientSession, ctx
}

// 端到端：真实协议拉取工具清单 → 建快照 → 按暴露名调用 → 拍平结果文本
func TestManager_端到端拉清单并调用(t *testing.T) {
	setupTestLogger(t)
	session, ctx := startInMemorySession(t)

	tools, err := listAllTools(ctx, session)
	if err != nil {
		t.Fatalf("拉取工具清单失败: %v", err)
	}
	if len(tools) != 2 {
		t.Fatalf("期望 2 个工具，得到 %d", len(tools))
	}

	m := newTestManager()
	attachLiveServer(m, "weather", session, tools...)
	m.rebuildSnapshot()

	// 中文名工具被规整成 tool，与 get_weather 不撞名
	if got := strings.Join(toolNames(m), ","); got != "get_weather,tool" {
		t.Fatalf("快照内容不对: %s", got)
	}
	// 服务端自动推导的 schema 要完整透传
	var weatherTool = m.Tools()[0]
	if !strings.Contains(string(weatherTool.ParamsJSON), `"city"`) {
		t.Fatalf("推导出的入参 schema 丢失: %s", weatherTool.ParamsJSON)
	}

	t.Run("正常调用", func(t *testing.T) {
		out, err := m.CallTool(ctx, "get_weather", `{"city":"北京"}`)
		if err != nil {
			t.Fatalf("调用失败: %v", err)
		}
		if out != "北京 晴，32℃" {
			t.Fatalf("结果不对: %q", out)
		}
	})

	t.Run("中文名工具按规整后的名字调用", func(t *testing.T) {
		out, err := m.CallTool(ctx, "tool", `{"city":"北京"}`)
		if err != nil {
			t.Fatalf("调用失败: %v", err)
		}
		if out != "路况畅通" {
			t.Fatalf("结果不对: %q", out)
		}
	})

	t.Run("服务端标记 isError 时同时给出文本和错误", func(t *testing.T) {
		out, err := m.CallTool(ctx, "get_weather", `{"city":""}`)
		if err == nil {
			t.Fatal("期望报错")
		}
		if out != "参数 city 不能为空" {
			t.Fatalf("工具自己的错误说明应带出，得到 %q", out)
		}
	})

	t.Run("模型给的入参不是合法 JSON", func(t *testing.T) {
		if _, err := m.CallTool(ctx, "get_weather", `{city: 北京`); err == nil {
			t.Fatal("期望报错")
		} else if !strings.Contains(err.Error(), "不是合法 JSON 对象") {
			t.Fatalf("错误信息不对: %v", err)
		}
	})

	t.Run("调用不存在的工具", func(t *testing.T) {
		if _, err := m.CallTool(ctx, "get_stock", `{}`); err == nil {
			t.Fatal("期望报错")
		}
	})

	t.Run("服务掉线后拒绝调用", func(t *testing.T) {
		m.servers[0].mu.Lock()
		m.servers[0].session = nil
		m.servers[0].mu.Unlock()
		// 故意不重建快照，模拟"快照还在但连接已断"的窗口期
		if _, err := m.CallTool(ctx, "get_weather", `{"city":"北京"}`); err == nil {
			t.Fatal("期望报错")
		} else if !strings.Contains(err.Error(), "当前未连接") {
			t.Fatalf("错误信息不对: %v", err)
		}
	})
}
