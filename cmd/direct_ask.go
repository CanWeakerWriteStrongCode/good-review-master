package cmd

import (
	"context"
	"log/slog"

	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/onebot"
)

func directAsk(event onebot.Event, groupID string, prompt string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), config.LLMTimeout)
		defer cancel()

		modelReply, err := llm.DefaultClient.Review(ctx, "你是什么大模型？", prompt)
		if err != nil {
			slog.Error("whoami 大模型调用失败", "err", err)
			modelReply = ""
		}
		result := "我是你忠实的好评大师"
		if modelReply != "" {
			result = result + "\n" + modelReply
		}
		onebot.SendGroupMessage(groupID, result)
	}()
}
