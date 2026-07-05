package config

import (
	"good-review-master/apppath"
	"good-review-master/logutil"
	"os"
)

// InitDefaultFiles 每次运行检测并补全缺失的模板配置文件。
// 独立检测 config.yaml 和 prompt_system.yaml，缺失则从内置模板创建。
func InitDefaultFiles() {
	// 检测并创建 config.yaml
	configPath := apppath.GetWorkPath("config.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		logutil.Info("未找到 config.yaml，正在从内置模板创建...")
		if err := os.WriteFile(configPath, configExampleTemplate, 0644); err != nil {
			logutil.Error("无法创建 config.yaml", "path", configPath, "err", err)
			os.Exit(1)
		}
		logutil.Info("已创建 config.yaml", "path", configPath)
	}

	// 检测并创建 prompt_system.yaml
	promptPath := apppath.GetWorkPath("prompt_system.yaml")
	if _, err := os.Stat(promptPath); os.IsNotExist(err) {
		logutil.Info("未找到 prompt_system.yaml，正在从内置模板创建...")
		if err := os.WriteFile(promptPath, promptSystemExampleTemplate, 0644); err != nil {
			logutil.Warn("无法创建 prompt_system.yaml", "path", promptPath, "err", err)
		} else {
			logutil.Info("已创建 prompt_system.yaml", "path", promptPath)
		}
	}
}
