package cmd

import (
	"context"
	"log/slog"

	"good-review-master/cache"
	"good-review-master/onebot"
)

// chatReview 异步锐评（作为 Router 的方法，通过 r 访问依赖）
func (r *Router) chatReview(event onebot.Event, groupID string, prompt string) {
	slog.Info("触发锐评", "group", groupID, "user", event.Nickname)
	go func() {
		msgs := cache.GetGroupCache(groupID, r.appCfg.MaxCacheMsg).GetAll()
		if len(msgs) == 0 {
			r.obClient.SendGroupMessage(groupID, "暂无群聊记录，无法锐评~")
			return
		}
		chatLog := cache.BuildChatLog(msgs)
		ctx, cancel := context.WithTimeout(context.Background(), r.appCfg.LLMTimeout)
		defer cancel()

		reply, err := r.llmClient.Review(ctx, chatLog, prompt)
		if err != nil {
			slog.Error("大模型调用失败", "err", err)
			r.obClient.SendGroupMessage(groupID, "大师今天罢工了，稍后再试~")
			return
		}
		r.obClient.SendGroupMessage(groupID, reply)
	}()
}
