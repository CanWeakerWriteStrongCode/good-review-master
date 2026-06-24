package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// CmdConf 命令配置（keyword + prompt）
type CmdConf struct {
	Keyword string `yaml:"keyword"`
	Prompt  string `yaml:"prompt"`
}

// CmdConfigs 所有命令配置（key=命令名，合并了 prompt.yaml + prompt_custom.yaml）
var CmdConfigs map[string][]CmdConf

type promptFile struct {
	Cmd   map[string][]CmdConf `yaml:"cmd"`
	Rules map[string]string    `yaml:"rules"`
}

// SharedRules prompt_system.yaml 中按命令类型定义的通用规则
var SharedRules map[string]string

func init() {
	loadPrompts()
}

func loadPrompts() {
	path := resolveConfigPath("prompt_system.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		slog.Error("无法读取 prompt.yaml", "path", path, "err", err)
		os.Exit(1)
	}
	var pf promptFile
	if err := yaml.Unmarshal(raw, &pf); err != nil {
		slog.Error("prompt.yaml 格式错误", "err", err)
		os.Exit(1)
	}
	if pf.Cmd == nil {
		pf.Cmd = make(map[string][]CmdConf)
	}
	CmdConfigs = pf.Cmd
	SharedRules = pf.Rules

	// 合并 prompt_custom.yaml
	customPath := customPromptPath()
	customRaw, err := os.ReadFile(customPath)
	if err != nil {
		return
	}
	var customPf promptFile
	if err := yaml.Unmarshal(customRaw, &customPf); err != nil {
		slog.Warn("prompt_custom.yaml 格式错误，跳过", "err", err)
		return
	}
	for k, v := range customPf.Cmd {
		CmdConfigs[k] = append(CmdConfigs[k], v...)
	}
}

// ReloadPrompts 热重载 prompt 配置
func ReloadPrompts() {
	loadPrompts()
}

// KeywordInMainPrompt 检查 keyword 是否已存在于 prompt.yaml（直接读文件校验）
func KeywordInMainPrompt(category, keyword string) bool {
	raw, err := os.ReadFile(resolveConfigPath("prompt_system.yaml"))
	if err != nil {
		return false
	}
	var pf promptFile
	if err := yaml.Unmarshal(raw, &pf); err != nil {
		return false
	}
	for _, e := range pf.Cmd[category] {
		if e.Keyword == keyword {
			return true
		}
	}
	return false
}

// AddPromptCommand 添加命令到 prompt_custom.yaml
func AddPromptCommand(category, keyword, promptText string) error {
	customPath := customPromptPath()
	var pf promptFile
	raw, err := os.ReadFile(customPath)
	if err == nil {
		if err := yaml.Unmarshal(raw, &pf); err != nil {
			return err
		}
	}
	if pf.Cmd == nil {
		pf.Cmd = make(map[string][]CmdConf)
	}

	pf.Cmd[category] = append(pf.Cmd[category], CmdConf{Keyword: keyword, Prompt: promptText})

	// 去重：同 category 下同 keyword 只保留最后一条
	seen := make(map[string]int)
	for i := len(pf.Cmd[category]) - 1; i >= 0; i-- {
		kw := pf.Cmd[category][i].Keyword
		if _, ok := seen[kw]; ok {
			pf.Cmd[category] = append(pf.Cmd[category][:i], pf.Cmd[category][i+1:]...)
		} else {
			seen[kw] = i
		}
	}

	return writePromptCustom(customPath, &pf)
}

// customPromptPath 返回 prompt_custom.yaml 的路径（与 prompt.yaml 同目录）
func customPromptPath() string {
	return filepath.Join(filepath.Dir(resolveConfigPath("prompt_system.yaml")), "prompt_custom.yaml")
}

// writePromptCustom 写入 prompt_custom.yaml，强制 prompt 使用 | 格式
func writePromptCustom(path string, pf *promptFile) error {
	var sb strings.Builder
	sb.WriteString("cmd:\n")
	for catName, entries := range pf.Cmd {
		sb.WriteString("  " + catName + ":\n")
		for _, e := range entries {
			sb.WriteString("    - keyword: \"" + e.Keyword + "\"\n")
			sb.WriteString("      prompt: |\n")
			for _, line := range strings.Split(e.Prompt, "\n") {
				sb.WriteString("        " + line + "\n")
			}
		}
	}
	return os.WriteFile(path, []byte(sb.String()), 0644)
}
