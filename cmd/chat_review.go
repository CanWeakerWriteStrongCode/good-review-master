package cmd

import (
	"context"
	"log/slog"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/onebot"
)

// chatReview 异步锐评
func chatReview(event onebot.Event, groupID string, prompt string) {
	slog.Info("触发锐评", "group", groupID, "user", event.Nickname)
	go func() {
		msgs := cache.GetGroupCache(groupID).GetAll()
		if len(msgs) == 0 {
			onebot.SendGroupMessage(groupID, "暂无群聊记录，无法锐评~")
			return
		}
		chatLog := cache.BuildChatLog(msgs)
		ctx, cancel := context.WithTimeout(context.Background(), config.LLMTimeout)
		defer cancel()

		reply, err := llm.DefaultClient.Review(ctx, chatLog, prompt)
		if err != nil {
			slog.Error("大模型调用失败", "err", err)
			onebot.SendGroupMessage(groupID, "大师今天罢工了，稍后再试~")
			return
		}
		onebot.SendGroupMessage(groupID, reply)
	}()
}
