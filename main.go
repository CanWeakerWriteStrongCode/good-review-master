package main

import (
	"log/slog"
	"os"

	"good-review-master/apppath"
	"good-review-master/bot"
	"good-review-master/cmd"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

func main() {
	logutil.SetupLogger()

	// 1. 加载主配置
	cfg, err := config.LoadConfig(apppath.ResolvePath("config.yaml"))
	if err != nil {
		slog.Error("加载配置失败", "err", err)
		os.Exit(1)
	}

	// 2. 加载提示词配置
	systemPromptPath := apppath.ResolvePath("prompt_system.yaml")
	customPromptPath := config.CustomPromptPath(systemPromptPath)
	promptCfg, err := config.LoadPromptConfig(systemPromptPath, customPromptPath)
	if err != nil {
		slog.Error("加载提示词配置失败", "err", err)
		os.Exit(1)
	}

	// 3. 创建大模型客户端
	var llmClient llm.Client
	switch cfg.LLMConfig.Provider {
	case "openai":
		llmClient = llm.NewOpenAIAdapter(
			cfg.LLMConfig.APIKey,
			cfg.LLMConfig.APIBase,
			cfg.LLMConfig.ModelName,
			cfg.LLMConfig.Temperature,
			cfg.LLMConfig.TopP,
		)
	default:
		slog.Error("不支持的大模型提供商", "provider", cfg.LLMConfig.Provider)
		os.Exit(1)
	}

	// 4. 创建 OneBot HTTP 客户端
	obClient := onebot.NewClient(cfg.NapCatHTTPAPI, cfg.NapCatAccessToken)

	// 5. 创建指令路由器
	router := cmd.NewRouter(cfg, promptCfg, llmClient, obClient)

	// 6. 获取机器人昵称
	if info, err := obClient.GetLoginInfo(); err != nil {
		slog.Warn("获取机器人昵称失败，@检测仅使用QQ号", "err", err)
	} else {
		cfg.BotNickname = info.Nickname
		slog.Info("机器人昵称", "nickname", cfg.BotNickname)
	}

	slog.Info("🚀 【不是好评大师】机器人启动成功")
	slog.Info("机器人QQ：" + cfg.BotQQ)
	slog.Info("允许响应群：" + cfg.AllowGroupsStr())
	slog.Info("NapCat HTTP API：" + cfg.NapCatHTTPAPI)

	// 7. 创建机器人并启动轮询
	botInstance := bot.NewBot(cfg, obClient, router)
	botInstance.RunPollingLoop()
}
