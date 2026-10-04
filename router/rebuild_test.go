package router

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/internal/testutil"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

// rebuildTestSystemYAML 一份最小的系统提示词：没有用户关键字，只有内部指令。
const rebuildTestSystemYAML = `
cmd: {}
rules: {}
`

// TestRebuild拾取提示词变化 是热更新的核心行为：
// Reload 只换数据，而路由表（前缀树 + 帮助列表）是从数据**派生**出来的，
// 不重建的话新关键字根本命中不了——用户看到的是"配置文件里明明写了，发出去却没反应"。
func TestRebuild拾取提示词变化(t *testing.T) {
	t.Chdir(t.TempDir()) // 日志写到临时 cwd/log，不污染仓库
	logutil.SetupLogger()
	t.Cleanup(logutil.Close)
	cache.ResetAll()

	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	customPath := config.CustomPromptPath(systemPath)
	if err := os.WriteFile(systemPath, []byte(rebuildTestSystemYAML), 0o600); err != nil {
		t.Fatalf("写系统提示词失败：%v", err)
	}

	promptCfg, err := config.LoadPromptConfig(systemPath, customPath)
	if err != nil {
		t.Fatalf("加载提示词失败：%v", err)
	}

	cfg := &config.Config{
		BotQQ:       "123456",
		BotNickname: "bot",
		MaxCacheMsg: 100,
		LLMTimeout:  5 * time.Second,
		LLMConfig:   config.LLMConf{MaxContextTokens: 50000},
	}
	instance := NewRouter(staticSnapshot(cfg), promptCfg,
		testutil.NewFakeLLM(), onebot.NewClient("http://127.0.0.1:1", ""), nil, context.Background())

	if matches(instance, "新增的词") {
		t.Fatal("配置里还没有这个词，不该命中")
	}

	// 模拟用户直接编辑 prompt_custom.yaml（不走内部指令）
	if err := os.WriteFile(customPath, []byte(`
cmd:
  chat_review:
    - keyword: "新增的词"
      prompt: "新加的"
`), 0o600); err != nil {
		t.Fatalf("写自定义提示词失败：%v", err)
	}

	// 热更新的两步：先重读数据，再重建派生出来的路由表
	promptCfg.Reload()
	instance.Rebuild()

	if !matches(instance, "新增的词") {
		t.Fatal("重载 + 重建后新关键字仍不命中——路由表没跟上数据")
	}
	// 帮助列表也应同步（它遍历的是同一份表）
	found := false
	for _, route := range instance.routesSnapshot() {
		if route.Keyword == "新增的词" {
			found = true
			break
		}
	}
	if !found {
		t.Error("帮助列表里看不到新关键字，说明列表与 trie 不是同一代")
	}
}

// TestRebuild保留内部指令 防一个容易犯的错：
// 内部指令只注册一次，若重建时忘了带上它们，热更新一次之后
// 「帮助」「添加关键字」这些就全没了。
func TestRebuild保留内部指令(t *testing.T) {
	t.Chdir(t.TempDir())
	logutil.SetupLogger()
	t.Cleanup(logutil.Close)

	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(systemPath, []byte(rebuildTestSystemYAML), 0o600); err != nil {
		t.Fatalf("写系统提示词失败：%v", err)
	}
	promptCfg, err := config.LoadPromptConfig(systemPath, config.CustomPromptPath(systemPath))
	if err != nil {
		t.Fatalf("加载提示词失败：%v", err)
	}

	cfg := &config.Config{BotQQ: "1", MaxCacheMsg: 10, LLMConfig: config.LLMConf{MaxContextTokens: 50000}}
	instance := NewRouter(staticSnapshot(cfg), promptCfg,
		testutil.NewFakeLLM(), onebot.NewClient("http://127.0.0.1:1", ""), nil, context.Background())

	internalBefore := countInternal(instance)
	if internalBefore == 0 {
		t.Fatal("启动后应有内部指令")
	}

	instance.Rebuild()
	instance.Rebuild() // 连重建两次

	if got := countInternal(instance); got != internalBefore {
		t.Fatalf("重建后内部指令从 %d 变成 %d——它们只注册一次，重建时必须原样带上",
			internalBefore, got)
	}
}

// TestRebuild不重复累积 防另一种错：若重建时往已有表上追加而不是新建，
// 每重建一次路由就会翻倍。
func TestRebuild不重复累积(t *testing.T) {
	t.Chdir(t.TempDir())
	logutil.SetupLogger()
	t.Cleanup(logutil.Close)

	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	customPath := config.CustomPromptPath(systemPath)
	if err := os.WriteFile(systemPath, []byte(rebuildTestSystemYAML), 0o600); err != nil {
		t.Fatalf("写系统提示词失败：%v", err)
	}
	if err := os.WriteFile(customPath, []byte("cmd:\n  chat_review:\n    - keyword: \"词\"\n      prompt: \"p\"\n"), 0o600); err != nil {
		t.Fatalf("写自定义提示词失败：%v", err)
	}

	promptCfg, err := config.LoadPromptConfig(systemPath, customPath)
	if err != nil {
		t.Fatalf("加载提示词失败：%v", err)
	}
	cfg := &config.Config{BotQQ: "1", MaxCacheMsg: 10, LLMConfig: config.LLMConf{MaxContextTokens: 50000}}
	instance := NewRouter(staticSnapshot(cfg), promptCfg,
		testutil.NewFakeLLM(), onebot.NewClient("http://127.0.0.1:1", ""), nil, context.Background())

	before := len(instance.routesSnapshot())
	for i := 0; i < 3; i++ {
		instance.Rebuild()
	}
	if got := len(instance.routesSnapshot()); got != before {
		t.Fatalf("路由条数从 %d 变成 %d：重建必须整体新建，不能往旧表上追加", before, got)
	}
}

func matches(instance *Router, text string) bool {
	return trieMatch(instance.table.Load().trie, text) != nil
}

func countInternal(instance *Router) int {
	count := 0
	for _, route := range instance.routesSnapshot() {
		if route.Category == "internal" {
			count++
		}
	}
	return count
}
