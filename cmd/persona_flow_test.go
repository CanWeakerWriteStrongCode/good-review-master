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

// TestPersonaReachesLLM 验证 persona 全链路：YAML 加载 → rebuild → RouteMessage
// → 发给 LLM 的 systemPrompt 必须携带【人格】块（身份/情绪维度/授权指令）。
// 覆盖用户关心的"发送时带没带人格"问题。
func TestPersonaReachesLLM(t *testing.T) {
	// 切到临时目录：SetupLogger 把日志写到 cwd/log/bot.log，避免污染仓库 cmd/log/
	t.Chdir(t.TempDir())
	logutil.SetupLogger() // 初始化 zap，否则 handler 里的日志调用会 panic
	defer logutil.Close() // 关闭日志文件句柄，释放临时目录供清理

	// 1. 写一个带 persona 的临时 prompt_system.yaml（结构与真实配置一致）
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	yml := `cmd:
  chat_review:
    - keyword: "锐评下"
      prompt: |
        做犀利总结。
      persona:
        identity: |
          毒舌点评人
        personality: '["一针见血"]'
        speech_style: '{"语气":"犀利"}'
        relationship: '{"角色":"点评员"}'
        greeting: |
          说吧
        system_prompt: |
          守住底线
        emotion: '{"情绪反应性":"高"}'
        examples: '[{"用户":"嗨","你":"说"}]'
rules:
  chat_review: |
    规则：禁止人身攻击
`
	if err := os.WriteFile(systemPath, []byte(yml), 0644); err != nil {
		t.Fatalf("写临时配置失败: %v", err)
	}

	// 2. 构造 Router + FakeLLM（不依赖真实大模型/NapCat）
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
	fakeLLM := testutil.NewFakeLLM()
	obClient := onebot.NewClient("http://127.0.0.1:1", "") // 死地址，调用失败仅日志
	router := NewRouter(cfg, promptCfg, fakeLLM, obClient, context.Background())

	// 3. 种入缓存消息，触发 锐评下
	cache.GetGroupCache("10001", cfg.MaxCacheMsg).Add(cache.Message{
		MsgID: 1, GroupID: "10001", UserID: "1", Nick: "张三", Content: "今天好累",
	})
	router.RouteMessage("[CQ:at,qq=123456] 锐评下", onebot.Event{
		PostType:    "message",
		MessageType: "group",
		GroupID:     "10001",
		UserID:      "1",
		Nickname:    "李四",
		RawMessage:  "[CQ:at,qq=123456] 锐评下",
		MessageID:   2,
	}, "10001")

	// 4. 等待异步 handler 完成 LLM 调用
	var calls []testutil.LLMCall
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		calls = fakeLLM.Calls()
		if len(calls) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(calls) == 0 {
		t.Fatal("FakeLLM 未被调用（异步 handler 未执行完成）")
	}

	// 5. 断言：人格移到 user 消息末尾（systemPrompt 保持稳定，跨指令前缀一致 → 不破坏聊天记录缓存命中）
	systemPrompt := calls[0].SystemPrompt
	chatLog := calls[0].ChatLog
	t.Logf("=== 发送给 LLM 的 systemPrompt ===\n%s", systemPrompt)
	t.Logf("=== 发送给 LLM 的 chatLog ===\n%s", chatLog)
	if strings.Contains(systemPrompt, "【人格】") {
		t.Fatalf("persona 不应在 systemPrompt（会破坏跨指令缓存前缀）:\n%s", systemPrompt)
	}
	if !strings.Contains(chatLog, "【人格】") {
		t.Fatalf("chatLog 未包含人格块:\n%s", chatLog)
	}
	if !strings.Contains(chatLog, "身份：毒舌点评人") {
		t.Fatalf("chatLog 未包含身份:\n%s", chatLog)
	}
	if !strings.Contains(chatLog, "情绪反应性") {
		t.Fatalf("chatLog 未包含情绪维度:\n%s", chatLog)
	}
	if !strings.Contains(chatLog, "推演每个维度应有的变化") {
		t.Fatalf("chatLog 未包含授权指令:\n%s", chatLog)
	}
	// 人格位于 user 消息末尾（聊天记录 + 关键词之后，保证聊天记录保持缓存前缀）
	if strings.Index(chatLog, "【人格】") <= strings.Index(chatLog, "做犀利总结。") {
		t.Fatalf("人格应位于关键词提示词之后（消息末尾）:\n%s", chatLog)
	}
}
