package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/internal/testutil"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

// TestSwitchPersona_* 验证 #切换人格/#取消人格：把"自带人格的关键字"记为本群当前人格，
// 仅对未命中关键字的纯 @ 聊天（replyDefault）生效；命中关键字的指令仍用自带人格。
// 人格渲染进 systemPrompt，user 消息（chatLog）不携带人格块（与 persona_flow_test 一致）。

const switchTestYAML = `cmd:
  chat_review:
    - keyword: "锐评下"
      prompt: |
        做犀利总结。
      persona:
        identity: |
          毒舌点评人
        personality: '["一针见血"]'
        relationship: '{"角色":"点评员"}'
        system_prompt: |
          守住底线
        emotion: '{"情绪反应性":"高"}'
    - keyword: "温柔"
      prompt: |
        温和地回应群友。
      persona:
        identity: |
          温柔大姐姐
        personality: '["耐心倾听"]'
        relationship: '{"角色":"知心姐姐"}'
        system_prompt: |
          语气轻柔
        emotion: '{"情绪反应性":"低"}'
rules:
  chat_review: |
    规则：禁止人身攻击
`

// switchTestEnv 测试环境：临时 prompt_system.yaml + Router + FakeLLM。
// groupID 用独立值并 ResetAll，避免全局 cache 与其它用例互相污染。
type switchTestEnv struct {
	router  *Router
	fake    *testutil.FakeLLM
	groupID string
}

func newSwitchTestEnv(t *testing.T) *switchTestEnv {
	t.Helper()
	t.Chdir(t.TempDir()) // 日志写到临时 cwd/log，不污染仓库
	logutil.SetupLogger()
	t.Cleanup(logutil.Close)
	cache.ResetAll()

	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(systemPath, []byte(switchTestYAML), 0644); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}
	promptCfg, err := config.LoadPromptConfig(systemPath, config.CustomPromptPath(systemPath))
	if err != nil {
		t.Fatalf("加载提示词配置失败: %v", err)
	}
	cfg := &config.Config{
		BotQQ:        "123456",
		BotNickname:  "bot",
		MaxCacheMsg:  100,
		LLMSendCount: 20,
		LLMTimeout:   5 * time.Second,
		LLMConfig: config.LLMConf{
			CacheHitCost:     0.033,
			CacheMissCost:    1.0,
			MaxContextTokens: 50000,
		},
	}
	fake := testutil.NewFakeLLM()
	obClient := onebot.NewClient("http://127.0.0.1:1", "") // 死地址，SendGroupMessage 仅日志
	router := NewRouter(cfg, promptCfg, fake, obClient, nil, context.Background())
	return &switchTestEnv{router: router, fake: fake, groupID: "20001"}
}

// seedMsg 种入一条群聊缓存消息（chatReview 空缓存会直接回复"暂无记录"而不调 LLM）
func (e *switchTestEnv) seedMsg(t *testing.T) {
	t.Helper()
	cache.GetGroupCache(e.groupID, 100).Add(cache.Message{
		MsgID: time.Now().UnixNano(), GroupID: e.groupID, UserID: "1", Nick: "张三", Content: "今天好累",
	})
}

func (e *switchTestEnv) route(t *testing.T, text string, msgID int64) {
	t.Helper()
	e.router.RouteMessage(text, onebot.Event{
		PostType:    "message",
		MessageType: "group",
		GroupID:     e.groupID,
		UserID:      "1",
		Nickname:    "李四",
		RawMessage:  text,
		MessageID:   msgID,
	}, e.groupID)
}

// waitLLMCalls 轮询直到 FakeLLM 累计调用数达到 wantTotal（内部指令不产生 LLM 调用）
func waitLLMCalls(t *testing.T, fake *testutil.FakeLLM, wantTotal int) []testutil.LLMCall {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if calls := fake.Calls(); len(calls) >= wantTotal {
			return calls
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("FakeLLM 调用数未达预期: want>=%d got=%d", wantTotal, len(fake.Calls()))
	return nil
}

// #切换人格 后，纯 @ 聊天把人格渲染进 systemPrompt，chatLog 不带人格
func TestSwitchPersona_DefaultChatInjectsGroupPersona(t *testing.T) {
	e := newSwitchTestEnv(t)
	e.seedMsg(t)

	e.route(t, "[CQ:at,qq=123456] #切换人格 锐评下", 1)

	b, ok := e.router.getGroupPersona(e.groupID)
	if !ok || b.keyword != "锐评下" {
		t.Fatalf("切换人格后群状态应为 锐评下, ok=%v binding=%+v", ok, b)
	}

	e.route(t, "[CQ:at,qq=123456] 今天心情怎么样", 2)
	calls := waitLLMCalls(t, e.fake, 1)
	sys, log := calls[0].SystemPrompt, calls[0].ChatLog
	if !strings.Contains(sys, "【人格】") || !strings.Contains(sys, "身份：毒舌点评人") {
		t.Fatalf("纯 @ 聊天应带群人格(毒舌点评人):\n%s", sys)
	}
	if strings.Contains(sys, "温柔大姐姐") {
		t.Fatalf("群人格应为 锐评下(毒舌点评人)，不应混入其它人格:\n%s", sys)
	}
	if strings.Contains(log, "【人格】") {
		t.Fatalf("persona 不应在 user 消息(chatLog)中:\n%s", log)
	}
}

// #取消人格 后，纯 @ 聊天恢复无人格
func TestSwitchPersona_CancelRestoresNormal(t *testing.T) {
	e := newSwitchTestEnv(t)
	e.seedMsg(t)

	e.route(t, "[CQ:at,qq=123456] #切换人格 锐评下", 1)
	e.route(t, "[CQ:at,qq=123456] #取消人格", 2)
	if _, ok := e.router.getGroupPersona(e.groupID); ok {
		t.Fatal("#取消人格 后群人格应被清空")
	}

	e.route(t, "[CQ:at,qq=123456] 随便聊聊", 3)
	calls := waitLLMCalls(t, e.fake, 1)
	if strings.Contains(calls[0].SystemPrompt, "【人格】") {
		t.Fatalf("取消人格后纯 @ 聊天不应再带人格块:\n%s", calls[0].SystemPrompt)
	}
}

// #切换人格 <不存在的人格> 不设置状态
func TestSwitchPersona_UnknownNameNotSet(t *testing.T) {
	e := newSwitchTestEnv(t)
	e.seedMsg(t)

	e.route(t, "[CQ:at,qq=123456] #切换人格 不存在的角色", 1)
	if _, ok := e.router.getGroupPersona(e.groupID); ok {
		t.Fatal("切换不存在的人格不应设置群状态")
	}

	e.route(t, "[CQ:at,qq=123456] 今天天气不错", 2)
	calls := waitLLMCalls(t, e.fake, 1)
	if strings.Contains(calls[0].SystemPrompt, "【人格】") {
		t.Fatalf("未设置人格时纯 @ 聊天不应带人格块:\n%s", calls[0].SystemPrompt)
	}
}

// 不带参数 #切换人格 只回用法，不产生 LLM 调用、不设置状态
func TestSwitchPersona_EmptyNameRepliesUsage(t *testing.T) {
	e := newSwitchTestEnv(t)

	e.route(t, "[CQ:at,qq=123456] #切换人格", 1)
	if _, ok := e.router.getGroupPersona(e.groupID); ok {
		t.Fatal("空参数 #切换人格 不应设置群状态")
	}
	if calls := e.fake.Calls(); len(calls) != 0 {
		t.Fatalf("内部指令不应触发 LLM 调用，got %d", len(calls))
	}
}

// 设了群人格后，命中关键字指令仍用该指令自带人格，不被群人格覆盖
func TestSwitchPersona_KeywordCommandKeepsOwnPersona(t *testing.T) {
	e := newSwitchTestEnv(t)
	e.seedMsg(t)

	// 群人格切到 锐评下（毒舌点评人）
	e.route(t, "[CQ:at,qq=123456] #切换人格 锐评下", 1)

	// 命中关键字 温柔（温柔大姐姐）→ 应使用指令自带人格，而非群人格
	e.route(t, "[CQ:at,qq=123456] 温柔 今天心情如何", 2)
	calls := waitLLMCalls(t, e.fake, 1)
	sys, log := calls[0].SystemPrompt, calls[0].ChatLog
	if !strings.Contains(sys, "身份：温柔大姐姐") {
		t.Fatalf("命中关键字应使用指令自带人格(温柔大姐姐):\n%s", sys)
	}
	if strings.Contains(sys, "毒舌点评人") {
		t.Fatalf("关键字指令不应被群人格(毒舌点评人)覆盖:\n%s", sys)
	}
	if strings.Contains(log, "【人格】") {
		t.Fatalf("persona 不应在 user 消息(chatLog)中:\n%s", log)
	}
}
