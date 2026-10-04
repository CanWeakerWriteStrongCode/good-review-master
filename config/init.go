package config

import (
	"os"

	"good-review-master/apppath"
	"good-review-master/logutil"
)

// secretFileMode secret.yaml 的权限：只给本用户读写。
// 这是它和 config.yaml（0644）刻意不同的一点——里面有 API Key 与登录密码。
const secretFileMode = 0o600

// InitDefaultFiles 每次运行检测并补全缺失的模板配置文件。
// 独立检测三个文件，缺失则从内置模板创建。
// 返回 true 表示 config.yaml 是本次新建的，调用方应提示用户配置后重新启动。
//
// 这是**唯一**允许创建文件的入口，刻意不放进 Load：
// 阶段 3 的 informer 会反复调 Load，那里必须无副作用。
func InitDefaultFiles() bool {
	configCreated := false

	// 检测并创建 config.yaml
	configPath := apppath.GetWorkPath("config.yaml")
	if !fileExists(configPath) {
		logutil.Info("未找到 config.yaml，正在从内置模板创建...")
		if err := os.WriteFile(configPath, configExampleTemplate, 0o644); err != nil {
			logutil.Error("无法创建 config.yaml", "path", configPath, "err", err)
			os.Exit(1)
		}
		logutil.Info("已创建 config.yaml", "path", configPath)
		configCreated = true
	}

	// 检测并创建 secret.yaml。它**不会**导致程序退出：
	// 密钥留空是合法状态（会回退到 config.yaml 的旧位置），不像 config.yaml 那样必须先去改。
	secretPath := apppath.GetWorkPath("secret.yaml")
	if !fileExists(secretPath) {
		logutil.Info("未找到 secret.yaml，正在从内置模板创建（密钥文件，请勿提交到版本库）...")
		if err := os.WriteFile(secretPath, secretExampleTemplate, secretFileMode); err != nil {
			logutil.Warn("无法创建 secret.yaml，密钥将继续从 config.yaml 读取",
				"path", secretPath, "err", err)
		} else {
			logutil.Info("已创建 secret.yaml", "path", secretPath)
		}
	}

	// 检测并创建 prompt_system.yaml
	promptPath := apppath.GetWorkPath("prompt_system.yaml")
	if !fileExists(promptPath) {
		logutil.Info("未找到 prompt_system.yaml，正在从内置模板创建...")
		if err := os.WriteFile(promptPath, promptSystemExampleTemplate, 0o644); err != nil {
			logutil.Warn("无法创建 prompt_system.yaml", "path", promptPath, "err", err)
		} else {
			logutil.Info("已创建 prompt_system.yaml", "path", promptPath)
		}
	}

	return configCreated
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
