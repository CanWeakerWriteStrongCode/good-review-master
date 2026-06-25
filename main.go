package main

import (
	"log/slog"

	"good-review-master/bot"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

func main() {
	logutil.SetupLogger()

	switch config.LLMConfig.Provider {
	case "openai":
		llm.DefaultClient = llm.NewOpenAIAdapter(
			config.LLMConfig.APIKey,
			config.LLMConfig.APIBase,
			config.LLMConfig.ModelName,
			config.LLMConfig.Temperature,
			config.LLMConfig.TopP,
		)
	default:
		slog.Error("不支持的大模型提供商")
		return
	}

	slog.Info("🚀 【不是好评大师】机器人启动成功")
	slog.Info("机器人QQ：" + config.BotQQ)
	slog.Info("允许响应群：" + config.AllowGroups)
	slog.Info("NapCat HTTP API：" + config.NapCatHTTPAPI)

	if info, err := onebot.GetLoginInfo(); err != nil {
		slog.Warn("获取机器人昵称失败，@检测仅使用QQ号", "err", err)
	} else {
		config.BotNickname = info.Nickname
		slog.Info("机器人昵称", "nickname", config.BotNickname)
	}

	bot.RunPollingLoop()
}
