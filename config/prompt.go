package config

import (
	"log/slog"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// CmdConf 命令配置（keyword + prompt）
type CmdConf struct {
	Keyword string `yaml:"keyword"`
	Prompt  string `yaml:"prompt"`
}

// CmdConfigs 所有命令配置（key=命令名）
var CmdConfigs map[string][]CmdConf

type promptFile struct {
	Cmd map[string][]CmdConf `yaml:"cmd"`
}

func init() {
	loadPrompts()
}

func loadPrompts() {
	path := resolveConfigPath("prompt.yaml")
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
	CmdConfigs = pf.Cmd
	if CmdConfigs == nil {
		CmdConfigs = make(map[string][]CmdConf)
	}

	// 合并 prompt_custom.yaml
	customPath := resolveConfigPath("prompt_custom.yaml")
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

// AddPromptCommand 添加命令到 prompt_custom.yaml
func AddPromptCommand(category, keyword, promptText string) error {
	customPath := resolveConfigPath("prompt_custom.yaml")
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
	return writePromptCustom(customPath, &pf)
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
