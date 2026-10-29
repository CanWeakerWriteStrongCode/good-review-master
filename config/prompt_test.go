package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// TestAddPersonaPrompt 验证 #添加人格 的生成提示词从 prompt_system.yaml 的
// internal.add_persona 读取；老配置没有这一段时退回内置默认值。
func TestAddPersonaPrompt(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	customPath := filepath.Join(dir, "prompt_custom.yaml")
	if err := os.WriteFile(systemPath, []byte(promptSystemYAML), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	pc, err := LoadPromptConfig(systemPath, customPath)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if got := pc.AddPersonaPrompt(); got != defaultAddPersonaPrompt {
		t.Errorf("缺少 internal.add_persona 时应退回内置默认值，实际 %q", got)
	}

	if err := os.WriteFile(systemPath, []byte(promptSystemYAML+"\ninternal:\n  add_persona: \"配置里的提示词\"\n"), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	pc.Reload()
	if got := pc.AddPersonaPrompt(); got != "配置里的提示词" {
		t.Errorf("应从 prompt_system.yaml 读取，实际 %q", got)
	}
}

// TestProactiveJudgePrompt 验证主动插话裁判提示词从 prompt_system.yaml 的
// internal.proactive_judge 读取；老配置没有这一段时退回内置默认值。
func TestProactiveJudgePrompt(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	customPath := filepath.Join(dir, "prompt_custom.yaml")
	if err := os.WriteFile(systemPath, []byte(promptSystemYAML), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	pc, err := LoadPromptConfig(systemPath, customPath)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if got := pc.ProactiveJudgePrompt(); got != defaultProactiveJudgePrompt {
		t.Errorf("缺少 internal.proactive_judge 时应退回内置默认值，实际 %q", got)
	}

	if err := os.WriteFile(systemPath, []byte(promptSystemYAML+"\ninternal:\n  proactive_judge: \"配置里的裁判提示词\"\n"), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	pc.Reload()
	if got := pc.ProactiveJudgePrompt(); got != "配置里的裁判提示词" {
		t.Errorf("应从 prompt_system.yaml 读取，实际 %q", got)
	}
}

// TestPersonaDirective 验证人格授权指令从 prompt_system.yaml 的
// internal.persona_directive 读取；老配置没有这一段时退回内置默认值。
func TestPersonaDirective(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	customPath := filepath.Join(dir, "prompt_custom.yaml")
	if err := os.WriteFile(systemPath, []byte(promptSystemYAML), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	pc, err := LoadPromptConfig(systemPath, customPath)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if got := pc.PersonaDirective(); got != defaultPersonaDirective {
		t.Errorf("缺少 internal.persona_directive 时应退回内置默认值，实际 %q", got)
	}

	if err := os.WriteFile(systemPath, []byte(promptSystemYAML+"\ninternal:\n  persona_directive: \"配置里的授权指令\"\n"), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	pc.Reload()
	if got := pc.PersonaDirective(); got != "配置里的授权指令" {
		t.Errorf("应从 prompt_system.yaml 读取，实际 %q", got)
	}
}

// TestFocusPrompt 验证焦点提示词二选一，且分别从 internal.focus_context / focus_last 读取。
func TestFocusPrompt(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	customPath := filepath.Join(dir, "prompt_custom.yaml")
	if err := os.WriteFile(systemPath, []byte(promptSystemYAML), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	pc, err := LoadPromptConfig(systemPath, customPath)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if got := pc.FocusPrompt(false); got != defaultFocusContext {
		t.Errorf("缺省应退回整体上下文兜底，实际 %q", got)
	}
	if got := pc.FocusPrompt(true); got != defaultFocusLast {
		t.Errorf("缺省应退回最后一条兜底，实际 %q", got)
	}

	if err := os.WriteFile(systemPath, []byte(promptSystemYAML+"\ninternal:\n  focus_context: \"看整体\"\n  focus_last: \"看最后\"\n"), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	pc.Reload()
	if got := pc.FocusPrompt(false); got != "看整体" {
		t.Errorf("应从 prompt_system.yaml 读取整体上下文提示，实际 %q", got)
	}
	if got := pc.FocusPrompt(true); got != "看最后" {
		t.Errorf("应从 prompt_system.yaml 读取最后一条提示，实际 %q", got)
	}
}

// TestReplyRequirement 验证 @ 回复的【要求】句从 prompt_system.yaml 的
// internal.reply_requirement 读取；老配置没有这一段时退回内置默认值。
func TestReplyRequirement(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	customPath := filepath.Join(dir, "prompt_custom.yaml")
	if err := os.WriteFile(systemPath, []byte(promptSystemYAML), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	pc, err := LoadPromptConfig(systemPath, customPath)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if got := pc.ReplyRequirement(); got != defaultReplyRequirement {
		t.Errorf("缺少 internal.reply_requirement 时应退回内置默认值，实际 %q", got)
	}

	if err := os.WriteFile(systemPath, []byte(promptSystemYAML+"\ninternal:\n  reply_requirement: \"配置里的要求句\"\n"), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	pc.Reload()
	if got := pc.ReplyRequirement(); got != "配置里的要求句" {
		t.Errorf("应从 prompt_system.yaml 读取，实际 %q", got)
	}
}

// TestProactiveRequirement 验证主动接话的【要求】句从 prompt_system.yaml 的
// internal.proactive_requirement 读取；老配置没有这一段时退回内置默认值。
func TestProactiveRequirement(t *testing.T) {
	dir := t.TempDir()
	systemPath := filepath.Join(dir, "prompt_system.yaml")
	customPath := filepath.Join(dir, "prompt_custom.yaml")
	if err := os.WriteFile(systemPath, []byte(promptSystemYAML), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	pc, err := LoadPromptConfig(systemPath, customPath)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if got := pc.ProactiveRequirement(); got != defaultProactiveRequirement {
		t.Errorf("缺少 internal.proactive_requirement 时应退回内置默认值，实际 %q", got)
	}

	if err := os.WriteFile(systemPath, []byte(promptSystemYAML+"\ninternal:\n  proactive_requirement: \"配置里的要求句\"\n"), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	pc.Reload()
	if got := pc.ProactiveRequirement(); got != "配置里的要求句" {
		t.Errorf("应从 prompt_system.yaml 读取，实际 %q", got)
	}
}

// TestPromptSystem示例含内置提示词 防止内嵌模板漏掉 internal.* 的生成/裁判提示词：
// 首次启动的用户拿到的是这份模板，缺了就只能吃兜底值、也没法在文件里改。
func TestPromptSystem示例含内置提示词(t *testing.T) {
	var cfg promptFile
	if err := yaml.Unmarshal(promptSystemExampleTemplate, &cfg); err != nil {
		t.Fatalf("内嵌模板解析失败：%v", err)
	}
	for _, key := range []string{
		"reply_requirement", "persona_directive", "add_persona", "proactive_judge",
		"proactive_requirement", "focus_context", "focus_last",
	} {
		if strings.TrimSpace(cfg.Internal[key]) == "" {
			t.Errorf("prompt_system_example.yaml 缺少 internal.%s", key)
		}
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
