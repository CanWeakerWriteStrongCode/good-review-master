package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// CmdConf 指令配置（keyword + prompt）
type CmdConf struct {
	Keyword string `yaml:"keyword"`
	Prompt  string `yaml:"prompt"`
}

// CmdConfigs 所有指令配置（key=指令名，合并了 prompt.yaml + prompt_custom.yaml）
var CmdConfigs map[string][]CmdConf

type promptFile struct {
	Cmd   map[string][]CmdConf `yaml:"cmd"`
	Rules map[string]string    `yaml:"rules"`
}

// SharedRules prompt_system.yaml 中按指令类型定义的通用规则
var SharedRules map[string]string

var promptMu sync.Mutex

func init() {
	loadPrompts()
}

func loadPrompts() {
	cfgPath := resolveConfigPath("prompt_system.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		slog.Error("无法读取 prompt.yaml", "path", cfgPath, "err", err)
		os.Exit(1)
	}
	var cfg promptFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		slog.Error("prompt.yaml 格式错误", "err", err)
		os.Exit(1)
	}
	if cfg.Cmd == nil {
		cfg.Cmd = make(map[string][]CmdConf)
	}
	CmdConfigs = cfg.Cmd
	SharedRules = cfg.Rules

	// 合并 prompt_custom.yaml
	customPath := customPromptPath()
	customRaw, err := os.ReadFile(customPath)
	if err != nil {
		return
	}
	var customCfg promptFile
	if err := yaml.Unmarshal(customRaw, &customCfg); err != nil {
		slog.Warn("prompt_custom.yaml 格式错误，跳过", "err", err)
		return
	}
	for name, entries := range customCfg.Cmd {
		CmdConfigs[name] = append(CmdConfigs[name], entries...)
	}
}

// ReloadPrompts 热重载 prompt 配置
func ReloadPrompts() {
	promptMu.Lock()
	defer promptMu.Unlock()
	loadPrompts()
}

// KeywordInMainPrompt 检查 keyword 是否已存在于 prompt.yaml（直接读文件校验）
func KeywordInMainPrompt(category, keyword string) bool {
	raw, err := os.ReadFile(resolveConfigPath("prompt_system.yaml"))
	if err != nil {
		return false
	}
	var cfg promptFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return false
	}
	for _, entry := range cfg.Cmd[category] {
		if entry.Keyword == keyword {
			return true
		}
	}
	return false
}

// KeywordInMainPromptAny 检查 keyword 是否在 prompt_system.yaml 任意 category 中存在
func KeywordInMainPromptAny(keyword string) bool {
	raw, err := os.ReadFile(resolveConfigPath("prompt_system.yaml"))
	if err != nil {
		return false
	}
	var cfg promptFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return false
	}
	for _, entries := range cfg.Cmd {
		for _, entry := range entries {
			if entry.Keyword == keyword {
				return true
			}
		}
	}
	return false
}

// DeletePromptCommand 从 prompt_custom.yaml 删除指令（按 keyword 全局匹配）
func DeletePromptCommand(keyword string) error {
	promptMu.Lock()
	defer promptMu.Unlock()
	customPath := customPromptPath()
	raw, err := os.ReadFile(customPath)
	if err != nil {
		return err
	}
	var cfg promptFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return err
	}
	for cat, entries := range cfg.Cmd {
		for i, entry := range entries {
			if entry.Keyword == keyword {
				cfg.Cmd[cat] = append(entries[:i], entries[i+1:]...)
				return writePromptCustom(customPath, &cfg)
			}
		}
	}
	return fmt.Errorf("未找到该指令: %s", keyword)
}

// AddPromptCommand 添加指令到 prompt_custom.yaml
func AddPromptCommand(category, keyword, promptText string) error {
	promptMu.Lock()
	defer promptMu.Unlock()
	customPath := customPromptPath()
	var cfg promptFile
	raw, err := os.ReadFile(customPath)
	if err == nil {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return err
		}
	}
	if cfg.Cmd == nil {
		cfg.Cmd = make(map[string][]CmdConf)
	}

	cfg.Cmd[category] = append(cfg.Cmd[category], CmdConf{Keyword: keyword, Prompt: promptText})

	// 去重：同 category 下同 keyword 只保留最后一条
	seen := make(map[string]int)
	for i := len(cfg.Cmd[category]) - 1; i >= 0; i-- {
		kw := cfg.Cmd[category][i].Keyword
		if _, ok := seen[kw]; ok {
			cfg.Cmd[category] = append(cfg.Cmd[category][:i], cfg.Cmd[category][i+1:]...)
		} else {
			seen[kw] = i
		}
	}

	return writePromptCustom(customPath, &cfg)
}

// customPromptPath 返回 prompt_custom.yaml 的路径（与 prompt.yaml 同目录）
func customPromptPath() string {
	return filepath.Join(filepath.Dir(resolveConfigPath("prompt_system.yaml")), "prompt_custom.yaml")
}

// writePromptCustom 写入 prompt_custom.yaml，强制 prompt 使用 | 格式
func writePromptCustom(path string, cfg *promptFile) error {
	var buf strings.Builder
	buf.WriteString("cmd:\n")
	for catName, entries := range cfg.Cmd {
		buf.WriteString("  " + catName + ":\n")
		for _, entry := range entries {
			buf.WriteString("    - keyword: \"" + entry.Keyword + "\"\n")
			buf.WriteString("      prompt: |\n")
			for _, line := range strings.Split(entry.Prompt, "\n") {
				buf.WriteString("        " + line + "\n")
			}
		}
	}
	return os.WriteFile(path, []byte(buf.String()), 0644)
}
