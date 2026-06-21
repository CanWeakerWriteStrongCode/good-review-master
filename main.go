package main

import (
	"log/slog"
	"os"

	"good-review-master/bot"
	"good-review-master/config"
	"good-review-master/llm"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})))

	switch config.LLMConfig.Provider {
	case "openai":
		llm.DefaultClient = llm.NewOpenAIAdapter(
			config.LLMConfig.APIKey,
			config.LLMConfig.APIBase,
			config.LLMConfig.ModelName,
			config.LLMConfig.Temperature,
		)
	default:
		slog.Error("不支持的大模型提供商")
		return
	}

	slog.Info("🚀 【不是好评大师】机器人启动成功")
	slog.Info("机器人QQ：" + config.BotQQ)
	slog.Info("允许响应群：" + config.AllowGroups)
	slog.Info("NapCat HTTP API：" + config.NapCatHTTPAPI)

	bot.RunPollingLoop()
}
