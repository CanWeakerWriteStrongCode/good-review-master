package config

import (
	"os"
	"path/filepath"
	"testing"
)

const promptSystemYAML = `
cmd:
  chat_review:
    - keyword: "锐评"
      prompt: "系统自带的锐评"
rules:
  chat_review: "系统规则"
`

func writePromptFiles(t *testing.T, customYAML string) (systemPath, customPath string) {
	t.Helper()
	dir := t.TempDir()
	systemPath = filepath.Join(dir, "prompt_system.yaml")
	customPath = filepath.Join(dir, "prompt_custom.yaml")
	if err := os.WriteFile(systemPath, []byte(promptSystemYAML), 0o600); err != nil {
		t.Fatalf("写入 prompt_system.yaml 失败：%v", err)
	}
	if err := os.WriteFile(customPath, []byte(customYAML), 0o600); err != nil {
		t.Fatalf("写入 prompt_custom.yaml 失败：%v", err)
	}
	return systemPath, customPath
}

func TestPromptConfig合并系统与自定义(t *testing.T) {
	systemPath, customPath := writePromptFiles(t, `
cmd:
  chat_review:
    - keyword: "自定义锐评"
      prompt: "自定义的"
rules:
  chat_review: "自定义规则"
`)
	pc, err := LoadPromptConfig(systemPath, customPath)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}

	data := pc.Snapshot()
	if data == nil {
		t.Fatal("Snapshot 不该为 nil（构造后必须已发布）")
	}
	entries := data.CmdConfigs["chat_review"]
	if len(entries) != 2 {
		t.Fatalf("系统 1 条 + 自定义 1 条 = 2 条，实际 %d：%v", len(entries), entries)
	}
	if data.SharedRules["chat_review"] != "自定义规则" {
		t.Errorf("同名类别下自定义规则应覆盖系统规则，实际 %q", data.SharedRules["chat_review"])
	}
}

// TestPromptConfig快照发布后不可变 是这个改动最要紧的性质。
//
// 背景：热重载让 Reload 与路由重建跑在不同 goroutine 上，而 Go 的 map 并发读写是
// **fatal error**（`concurrent map read and map write`），不是竞态警告那么轻。
// 只要"发布后不再改动"这条成立，读侧就永远安全；一旦有人把 Reload 改回就地改 map，
// 这条测试会立刻红。
func TestPromptConfig快照发布后不可变(t *testing.T) {
	systemPath, customPath := writePromptFiles(t, `
cmd:
  chat_review:
    - keyword: "旧关键字"
      prompt: "旧的"
`)
	pc, err := LoadPromptConfig(systemPath, customPath)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}

	before := pc.Snapshot()
	// 系统文件贡献「锐评」1 条，自定义文件贡献「旧关键字」1 条，合并后 2 条
	if len(before.CmdConfigs["chat_review"]) != 2 {
		t.Fatalf("初始应有 2 条（系统 1 + 自定义 1），实际 %d", len(before.CmdConfigs["chat_review"]))
	}

	// 改盘上的自定义文件再重载
	if err := os.WriteFile(customPath, []byte(`
cmd:
  chat_review:
    - keyword: "旧关键字"
      prompt: "旧的"
    - keyword: "新关键字"
      prompt: "新的"
`), 0o600); err != nil {
		t.Fatalf("改写失败：%v", err)
	}
	pc.Reload()

	after := pc.Snapshot()
	if len(after.CmdConfigs["chat_review"]) != 3 {
		t.Fatalf("重载后应有 3 条，实际 %d", len(after.CmdConfigs["chat_review"]))
	}
	// 关键断言：旧快照必须原封不动（若 Reload 就地改 map，这里会变成 3）
	if len(before.CmdConfigs["chat_review"]) != 2 {
		t.Fatalf("旧快照被就地改动了（变成 %d 条）：Reload 必须整体替换而不是改写原 map",
			len(before.CmdConfigs["chat_review"]))
	}
	if before == after {
		t.Fatal("重载后 Snapshot 应返回新对象")
	}
}

// TestPromptConfig自定义不污染系统守卫 锁住"系统文件"与"合并结果"两份数据的分工。
// 守卫问的是"这个词在系统文件里吗"——若拿合并后的数据来判，
// 用户自己加的指令会被误判成系统指令而无法再修改删除。
func TestPromptConfig自定义不污染系统守卫(t *testing.T) {
	systemPath, customPath := writePromptFiles(t, `
cmd:
  chat_review:
    - keyword: "用户自己的词"
      prompt: "x"
`)
	pc, err := LoadPromptConfig(systemPath, customPath)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}

	if pc.KeywordInSystemCmd("锐评") != true {
		t.Error("系统里的关键字应判为 true")
	}
	if pc.KeywordInSystemCmd("用户自己的词") != false {
		t.Error("自定义关键字不该被当成系统关键字——否则它就再也删不掉了")
	}
	if pc.CategoryInSystemRule("chat_review") != true {
		t.Error("系统里的规则类别应判为 true")
	}
}

// TestPromptConfig缺自定义文件也能用 保证首次运行（prompt_custom.yaml 尚未创建）正常。
func TestPromptConfig缺自定义文件也能用(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(systemPath, []byte(promptSystemYAML), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	pc, err := LoadPromptConfig(systemPath, filepath.Join(dir, "不存在.yaml"))
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	data := pc.Snapshot()
	if data == nil || len(data.CmdConfigs["chat_review"]) != 1 {
		t.Fatalf("应只有系统的 1 条，实际 %+v", data)
	}
}

// TestPromptConfig畸形自定义文件不致命 保证用户把 YAML 写坏了也能用系统提示词启动。
func TestPromptConfig畸形自定义文件不致命(t *testing.T) {
	systemPath, customPath := writePromptFiles(t, "cmd: [这不是映射]\n  bad: {{{\n")
	pc, err := LoadPromptConfig(systemPath, customPath)
	if err != nil {
		t.Fatalf("自定义文件格式错不该让加载失败：%v", err)
	}
	if len(pc.Snapshot().CmdConfigs["chat_review"]) != 1 {
		t.Error("应回退到只有系统指令")
	}
}
