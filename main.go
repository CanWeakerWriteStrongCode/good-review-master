package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

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
		logutil.Error("加载配置失败", "err", err)
		os.Exit(1)
	}

	// 2. 加载提示词配置
	systemPromptPath := apppath.ResolvePath("prompt_system.yaml")
	customPromptPath := config.CustomPromptPath(systemPromptPath)
	promptCfg, err := config.LoadPromptConfig(systemPromptPath, customPromptPath)
	if err != nil {
		logutil.Error("加载提示词配置失败", "err", err)
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
		logutil.Error("不支持的大模型提供商", "provider", cfg.LLMConfig.Provider)
		os.Exit(1)
	}

	// 4. 创建 OneBot HTTP 客户端
	obClient := onebot.NewClient(cfg.NapCatHTTPAPI, cfg.NapCatAccessToken)

	// 5. 创建指令路由器（传入 shutdown context，goroutine 通过 errgroup 自动继承）
	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	router := cmd.NewRouter(cfg, promptCfg, llmClient, obClient, shutdownCtx)

	// 6. 获取机器人昵称
	if info, err := obClient.GetLoginInfo(); err != nil {
		logutil.Warn("获取机器人昵称失败，@检测仅使用QQ号", "err", err)
	} else {
		cfg.BotNickname = info.Nickname
		logutil.Info("机器人昵称", "nickname", cfg.BotNickname)
	}

	logutil.Info("🚀 【不是好评大师】机器人启动成功")
	logutil.Info("机器人QQ：" + cfg.BotQQ)
	logutil.Info("允许响应群：" + cfg.AllowGroupsStr())
	logutil.Info("NapCat HTTP API：" + cfg.NapCatHTTPAPI)

	// 7. 创建机器人并启动轮询（支持优雅退出）
	botInstance := bot.NewBot(cfg, obClient, router)
	go botInstance.RunPollingLoop(shutdownCtx)

	<-shutdownCtx.Done()
	logutil.Info("收到退出信号，正在关闭...")
	if err := router.Wait(); err != nil {
		logutil.Error("等待 goroutine 退出失败", "err", err)
	}
	logutil.Info("已安全退出")
}
