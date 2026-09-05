package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/internal/testutil"
	"good-review-master/llm"
	"good-review-master/logutil"
)

// setupTestLogger 初始化 zap 并切到临时目录：logutil 的 sugar 未初始化时任何日志调用都会
// nil panic，而工具循环与选窗决策里都有日志；不切目录会把 bot.log 写进仓库 cmd/log/。
func setupTestLogger(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	logutil.SetupLogger()
	t.Cleanup(logutil.Close)
}

// fakeMCP 测试用的 MCPProvider：固定工具清单 + 可编程的调用结果
type fakeMCP struct {
	tools   []llm.Tool
	tokens  int
	results map[string]string // 工具名 → 返回文本
	errs    map[string]error  // 工具名 → 调用错误（与 results 同时配置时模拟 MCP isError）
	calls   []string          // 调用顺序，形如 name(args)
}

func (f *fakeMCP) Tools() []llm.Tool { return f.tools }

func (f *fakeMCP) ToolsTokens() int { return f.tokens }

func (f *fakeMCP) CallTool(_ context.Context, name, argsJSON string) (string, error) {
	f.calls = append(f.calls, name+"("+argsJSON+")")
	out := f.results[name]
	if err, ok := f.errs[name]; ok {
		return out, err
	}
	if _, ok := f.results[name]; !ok {
		return "", errors.New("工具不存在或已下线: " + name)
	}
	return out, nil
}

// newToolLoopRouter 直接拼 Router（不经 NewRouter）：工具循环只用到 llmClient / mcp / appCfg 三项
func newToolLoopRouter(fakeLLM *testutil.FakeLLM, provider MCPProvider, maxRounds, maxResultRune int) *Router {
	return &Router{
		llmClient: fakeLLM,
		mcp:       provider,
		appCfg: &config.Config{
			LLMTimeout: 5 * time.Second,
			MCPConfig: config.MCPConf{
				MaxToolRounds:     maxRounds,
				MaxToolResultRune: maxResultRune,
				ToolTimeout:       time.Second,
			},
		},
	}
}

// weatherMCP 一个带 get_weather 工具的假服务
func weatherMCP(result string) *fakeMCP {
	return &fakeMCP{
		tools: []llm.Tool{{
			Name:        "get_weather",
			Description: "查询指定城市的实时天气",
			ParamsJSON:  []byte(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
		tokens:  42,
		results: map[string]string{"get_weather": result},
	}
}

// 模型第一轮要求调工具、第二轮给出文本答案：
// 断言工具确实被执行、结果以 role=tool 回填且 ToolCallID 与发起时一致。
func TestRunChatWithTools_单轮工具调用后作答(t *testing.T) {
	setupTestLogger(t)
	fakeLLM := testutil.NewFakeLLM()
	fakeLLM.SetChatScript(
		&llm.ChatResponse{ToolCalls: []llm.ToolCall{
			{ID: "call_abc123", Name: "get_weather", Arguments: `{"city":"北京"}`},
		}},
		&llm.ChatResponse{Content: "北京 32 度大晴天，晒得慌。"},
	)
	mcp := weatherMCP("晴，32℃，东南风 2 级")
	r := newToolLoopRouter(fakeLLM, mcp, 5, 2000)

	reply, err := r.runChatWithTools(context.Background(), "你是锐评大师", "以下是群聊记录：\n张三：今天好热")
	if err != nil {
		t.Fatalf("期望成功，得到错误: %v", err)
	}
	if reply != "北京 32 度大晴天，晒得慌。" {
		t.Fatalf("回复不对，得到: %q", reply)
	}

	if len(mcp.calls) != 1 || mcp.calls[0] != `get_weather({"city":"北京"})` {
		t.Fatalf("工具调用记录不对: %v", mcp.calls)
	}

	chatCalls := fakeLLM.ChatCalls()
	if len(chatCalls) != 2 {
		t.Fatalf("期望 2 次 Chat 调用，得到 %d", len(chatCalls))
	}
	// 第一轮就带上了工具清单
	if len(chatCalls[0].ToolNames) != 1 || chatCalls[0].ToolNames[0] != "get_weather" {
		t.Fatalf("第一轮未下发工具清单: %v", chatCalls[0].ToolNames)
	}
	// 第二轮的消息序列：system → user → assistant(tool_calls) → tool(结果)
	msgs := chatCalls[1].Messages
	wantRoles := []string{llm.RoleSystem, llm.RoleUser, llm.RoleAssistant, llm.RoleTool}
	if len(msgs) != len(wantRoles) {
		t.Fatalf("第二轮消息数期望 %d，得到 %d", len(wantRoles), len(msgs))
	}
	for i, want := range wantRoles {
		if msgs[i].Role != want {
			t.Fatalf("第 %d 条消息 role 期望 %s，得到 %s", i, want, msgs[i].Role)
		}
	}
	if len(msgs[2].ToolCalls) != 1 || msgs[2].ToolCalls[0].ID != "call_abc123" {
		t.Fatalf("assistant 消息未带回 tool_calls: %+v", msgs[2].ToolCalls)
	}
	if msgs[3].ToolCallID != "call_abc123" {
		t.Fatalf("tool 消息的 ToolCallID 期望 call_abc123，得到 %q", msgs[3].ToolCallID)
	}
	if msgs[3].Name != "get_weather" {
		t.Fatalf("tool 消息的 Name 期望 get_weather，得到 %q", msgs[3].Name)
	}
	if msgs[3].Content != "晴，32℃，东南风 2 级" {
		t.Fatalf("工具结果未原样回填，得到 %q", msgs[3].Content)
	}
}

// 模型一直要求调工具：达到 max_tool_rounds 后必须撤回 tools 字段强制作答，不能无限循环。
func TestRunChatWithTools_轮数上限后撤回工具(t *testing.T) {
	setupTestLogger(t)
	fakeLLM := testutil.NewFakeLLM()
	fakeLLM.SetReply("行吧，不查了，我猜是晴天。")
	askAgain := &llm.ChatResponse{ToolCalls: []llm.ToolCall{
		{ID: "call_1", Name: "get_weather", Arguments: `{"city":"北京"}`},
	}}
	// 前两轮都要调工具；第三轮脚本耗尽，FakeLLM 回落到固定文本回复
	fakeLLM.SetChatScript(askAgain, askAgain)
	mcp := weatherMCP("晴，32℃")
	r := newToolLoopRouter(fakeLLM, mcp, 2, 2000)

	reply, err := r.runChatWithTools(context.Background(), "你是锐评大师", "以下是群聊记录：\n张三：今天好热")
	if err != nil {
		t.Fatalf("期望成功，得到错误: %v", err)
	}
	if reply != "行吧，不查了，我猜是晴天。" {
		t.Fatalf("回复不对，得到: %q", reply)
	}

	chatCalls := fakeLLM.ChatCalls()
	if len(chatCalls) != 3 {
		t.Fatalf("期望 3 次 Chat 调用（2 轮工具 + 1 轮强制作答），得到 %d", len(chatCalls))
	}
	if len(chatCalls[0].ToolNames) != 1 || len(chatCalls[1].ToolNames) != 1 {
		t.Fatal("前两轮应该都下发工具清单")
	}
	if chatCalls[2].ToolNames != nil {
		t.Fatalf("第三轮必须撤回工具清单，实际下发: %v", chatCalls[2].ToolNames)
	}
	if len(mcp.calls) != 2 {
		t.Fatalf("工具应只被调用 2 次，实际 %d 次: %v", len(mcp.calls), mcp.calls)
	}
}

// 工具执行失败：失败原因要作为 role=tool 消息回填给模型（让它自纠），而不是中断整个对话。
func TestRunChatWithTools_工具报错回填给模型(t *testing.T) {
	setupTestLogger(t)
	t.Run("传输层错误且无输出", func(t *testing.T) {
		fakeLLM := testutil.NewFakeLLM()
		fakeLLM.SetChatScript(
			&llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{}`}}},
			&llm.ChatResponse{Content: "查不到天气，但肯定很热。"},
		)
		mcp := weatherMCP("")
		mcp.errs = map[string]error{"get_weather": errors.New("连接超时")}
		r := newToolLoopRouter(fakeLLM, mcp, 5, 2000)

		if _, err := r.runChatWithTools(context.Background(), "s", "u"); err != nil {
			t.Fatalf("工具失败不该中断对话，得到错误: %v", err)
		}
		got := fakeLLM.ChatCalls()[1].Messages[3].Content
		if got != "工具调用失败：连接超时" {
			t.Fatalf("回填内容不对: %q", got)
		}
	})

	t.Run("MCP isError 带工具自己的错误说明", func(t *testing.T) {
		fakeLLM := testutil.NewFakeLLM()
		fakeLLM.SetChatScript(
			&llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{}`}}},
			&llm.ChatResponse{Content: "哦，得先给城市名。"},
		)
		mcp := weatherMCP("参数 city 不能为空")
		mcp.errs = map[string]error{"get_weather": errors.New("工具执行报错")}
		r := newToolLoopRouter(fakeLLM, mcp, 5, 2000)

		if _, err := r.runChatWithTools(context.Background(), "s", "u"); err != nil {
			t.Fatalf("期望成功，得到错误: %v", err)
		}
		got := fakeLLM.ChatCalls()[1].Messages[3].Content
		if got != "参数 city 不能为空" {
			t.Fatalf("工具自己的错误说明应原样交给模型，得到: %q", got)
		}
	})

	t.Run("模型幻觉出不存在的工具", func(t *testing.T) {
		fakeLLM := testutil.NewFakeLLM()
		fakeLLM.SetChatScript(
			&llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "get_stock", Arguments: `{}`}}},
			&llm.ChatResponse{Content: "没这工具，那我不查了。"},
		)
		r := newToolLoopRouter(fakeLLM, weatherMCP("晴"), 5, 2000)

		if _, err := r.runChatWithTools(context.Background(), "s", "u"); err != nil {
			t.Fatalf("期望成功，得到错误: %v", err)
		}
		got := fakeLLM.ChatCalls()[1].Messages[3].Content
		if !strings.HasPrefix(got, "工具调用失败：工具不存在或已下线: get_stock") {
			t.Fatalf("回填内容不对: %q", got)
		}
	})
}

// 工具返回超长结果必须按 max_tool_result_rune 截断，防止撑爆上下文护栏。
func TestRunChatWithTools_工具结果按字符数截断(t *testing.T) {
	setupTestLogger(t)
	fakeLLM := testutil.NewFakeLLM()
	fakeLLM.SetChatScript(
		&llm.ChatResponse{ToolCalls: []llm.ToolCall{{ID: "call_1", Name: "get_weather", Arguments: `{}`}}},
		&llm.ChatResponse{Content: "看完了。"},
	)
	long := strings.Repeat("晴", 30)
	r := newToolLoopRouter(fakeLLM, weatherMCP(long), 5, 10)

	if _, err := r.runChatWithTools(context.Background(), "s", "u"); err != nil {
		t.Fatalf("期望成功，得到错误: %v", err)
	}
	got := fakeLLM.ChatCalls()[1].Messages[3].Content
	if want := strings.Repeat("晴", 10) + "..."; got != want {
		t.Fatalf("截断结果不对，期望 %q，得到 %q", want, got)
	}
}

// 模型既不给文本也不给工具调用时不能把空消息发到群里
func TestRunChatWithTools_空回复算失败(t *testing.T) {
	setupTestLogger(t)
	fakeLLM := testutil.NewFakeLLM()
	fakeLLM.SetChatScript(&llm.ChatResponse{Content: "   "})
	r := newToolLoopRouter(fakeLLM, weatherMCP("晴"), 5, 2000)

	if _, err := r.runChatWithTools(context.Background(), "s", "u"); !errors.Is(err, errEmptyReply) {
		t.Fatalf("期望 errEmptyReply，得到: %v", err)
	}
}

// 没有可用工具时必须走原来的单轮 Review：请求体不带 tools 字段，与改造前字节一致，不无故击穿缓存。
func TestRunReviewLLM_无工具时走单轮调用(t *testing.T) {
	setupTestLogger(t)
	cases := []struct {
		name     string
		provider MCPProvider
	}{
		{"MCP 未启用（provider 为 nil）", nil},
		{"服务全挂（工具清单为空）", &fakeMCP{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeLLM := testutil.NewFakeLLM()
			fakeLLM.SetReply("单轮回复")
			r := newToolLoopRouter(fakeLLM, tc.provider, 5, 2000)

			reply, err := r.runReviewLLM(context.Background(), "系统提示", "用户消息")
			if err != nil {
				t.Fatalf("期望成功，得到错误: %v", err)
			}
			if reply != "单轮回复" {
				t.Fatalf("回复不对: %q", reply)
			}
			if len(fakeLLM.ChatCalls()) != 0 {
				t.Fatalf("不该走 Chat 多轮路径，实际走了 %d 次", len(fakeLLM.ChatCalls()))
			}
			calls := fakeLLM.Calls()
			if len(calls) != 1 || calls[0].ChatLog != "用户消息" || calls[0].SystemPrompt != "系统提示" {
				t.Fatalf("Review 调用参数不对: %+v", calls)
			}
		})
	}
}

// 有工具时 runReviewLLM 必须切到工具循环
func TestRunReviewLLM_有工具时走工具循环(t *testing.T) {
	setupTestLogger(t)
	fakeLLM := testutil.NewFakeLLM()
	fakeLLM.SetReply("带工具的回复")
	r := newToolLoopRouter(fakeLLM, weatherMCP("晴"), 5, 2000)

	if _, err := r.runReviewLLM(context.Background(), "系统提示", "用户消息"); err != nil {
		t.Fatalf("期望成功，得到错误: %v", err)
	}
	if len(fakeLLM.ChatCalls()) != 1 {
		t.Fatalf("期望走 Chat 多轮路径 1 次，得到 %d", len(fakeLLM.ChatCalls()))
	}
	if len(fakeLLM.Calls()) != 0 {
		t.Fatal("不该走单轮 Review 路径")
	}
}

// 工具清单的 token 必须计入缓存成本模型的固定前缀，
// 否则会低估总 token，把扩展窗口撑到撞穿 max_context_tokens 护栏。
func TestSelectChatWindow_工具token计入上下文护栏(t *testing.T) {
	setupTestLogger(t)
	// 10 条消息；护栏与断言按实际 ChatLogTokens 实时计算，避免硬编码消息格式相关的数字
	msgs := make([]cache.Message, 0, 10)
	for i := 0; i < 10; i++ {
		msgs = append(msgs, cache.Message{
			MsgID: int64(i + 1), GroupID: "10001", UserID: "1", Nick: "张三", Content: "今天好累啊",
		})
	}
	groupID := "guard-test-group"
	// 本用例 selectChatWindow 的 systemPrompt 传空串；锚点覆盖前 5 条
	baseSystemTokens := cache.EstimateTokens("")
	perMsgTokens := cache.ChatLogTokens(msgs[:1])
	// 护栏：恰好装下全部 10 条（无工具时）→ 不带工具应能扩展
	guardTokens := baseSystemTokens + 10*perMsgTokens

	buildRouter := func(provider MCPProvider, maxContextTokens int) *Router {
		return &Router{
			mcp: provider,
			appCfg: &config.Config{
				LLMSendCount: 20,
				MCPConfig:    config.MCPConf{MaxToolRounds: 5},
				LLMConfig: config.LLMConf{
					CacheHitCost:     0.033,
					CacheMissCost:    1.0,
					MaxContextTokens: maxContextTokens,
				},
			},
		}
	}

	t.Run("不带工具时护栏内可扩展", func(t *testing.T) {
		cache.ResetAll()
		cache.SetLLMAnchor(groupID, cache.LLMAnchor{Start: 1, LastSent: 5})
		// 命中 5*perMsg + 新增 5*perMsg = 10*perMsg，恰 <= 护栏 → 扩展全部 10 条
		got := buildRouter(nil, guardTokens).selectChatWindow(msgs, groupID, "")
		if len(got) != 10 {
			t.Fatalf("期望扩展到全部 10 条，得到 %d 条", len(got))
		}
	})

	t.Run("工具清单顶穿护栏后强制重置", func(t *testing.T) {
		cache.ResetAll()
		cache.SetLLMAnchor(groupID, cache.LLMAnchor{Start: 1, LastSent: 5})
		// 同样的消息与锚点，只多 100 token 的工具清单 → 固定前缀超出护栏 → 必须重置并截断
		mcpTokens := 100
		r := buildRouter(&fakeMCP{tokens: mcpTokens}, guardTokens)
		got := r.selectChatWindow(msgs, groupID, "")
		if len(got) == 10 {
			t.Fatal("工具 token 未计入护栏，窗口没有收缩")
		}
		// 截断应保留最新消息
		if got[len(got)-1].MsgID != 10 {
			t.Fatalf("重置应保留最新的消息，得到最后 MsgID=%d", got[len(got)-1].MsgID)
		}
		// 截断后的窗口（前缀 = systemPrompt + 工具清单）不得再超护栏
		systemTokens := baseSystemTokens + mcpTokens
		if actual := systemTokens + cache.ChatLogTokens(got); actual > guardTokens {
			t.Fatalf("截断后仍超护栏: token=%d > guard=%d", actual, guardTokens)
		}
	})
	cache.ResetAll()
}
