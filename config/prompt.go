package config

import (
	"log/slog"
	"os"

	"gopkg.in/yaml.v3"
)

// CmdConf 命令配置（keyword + prompt）
type CmdConf struct {
	Keyword string `yaml:"keyword"`
	Prompt  string `yaml:"prompt"`
}

// CmdConfigs 所有命令配置（key=命令名）
var CmdConfigs map[string]CmdConf

type promptFile struct {
	Cmd map[string]CmdConf `yaml:"cmd"`
}

func init() {
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
}
