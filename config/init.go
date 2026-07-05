package config

import (
	"good-review-master/apppath"
	"good-review-master/logutil"
	"os"
)

// InitDefaultFiles 首次运行初始化所有模板配置文件。
// 以 config.yaml 是否存在为判断依据：不存在则视为首次运行，批量创建所有模板文件。
// 返回 true 表示创建了文件（首次运行），调用方应提示用户并退出。
func InitDefaultFiles() bool {
	configPath := apppath.GetWorkPath("config.yaml")
	if _, err := os.Stat(configPath); err == nil {
		return false
	}

	logutil.Info("检测到首次运行，正在创建默认配置文件...")

	// 创建 config.yaml
	if err := os.WriteFile(configPath, configExampleTemplate, 0644); err != nil {
		logutil.Error("无法创建 config.yaml", "path", configPath, "err", err)
		os.Exit(1)
	}
	logutil.Info("已创建 config.yaml", "path", configPath)

	// 创建 prompt_system.yaml
	promptPath := apppath.GetWorkPath("prompt_system.yaml")
	if _, err := os.Stat(promptPath); os.IsNotExist(err) {
		if err := os.WriteFile(promptPath, promptSystemExampleTemplate, 0644); err != nil {
			logutil.Warn("无法创建 prompt_system.yaml", "path", promptPath, "err", err)
		} else {
			logutil.Info("已创建 prompt_system.yaml", "path", promptPath)
		}
	}

	return true
}
