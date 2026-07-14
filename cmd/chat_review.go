package cmd

import (
	"context"
	"good-review-master/logutil"

	"good-review-master/cache"
	"good-review-master/onebot"
)

// chatReview 异步锐评（通过 async 管理生命周期，自动继承 shutdown context）
func (r *Router) chatReview(event onebot.Event, groupID string, systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	logutil.Info("触发锐评", "group", groupID, "user", event.Nickname)
	r.Go(func(ctx context.Context) error {
		msgs := cache.GetGroupCache(groupID, r.appCfg.MaxCacheMsg).GetAll()
		if len(msgs) == 0 {
			r.obClient.SendGroupMessage(groupID, "暂无群聊记录，无法锐评~")
			return nil
		}

		// Phase 2: 缓存扩展窗口（同 category 下所有关键词共享锚点）
		var chatLogMsgs []cache.Message
		anchor := cache.GetLLMAnchor(groupID)
		if anchor != nil {
			startIdx := cache.FindMsgIndex(msgs, int64(*anchor))
			if startIdx >= 0 {
				chatLogMsgs = msgs[startIdx:]
				logutil.Debug("缓存扩展", "group", groupID, "窗口", len(chatLogMsgs))
			}
		}
		if len(chatLogMsgs) == 0 {
			startIdx := len(msgs) - r.appCfg.LLMSendCount
			if startIdx < 0 {
				startIdx = 0
			}
			chatLogMsgs = msgs[startIdx:]
		}

		chatLog := cache.BuildChatLog(chatLogMsgs)
		ctx, cancel := context.WithTimeout(ctx, r.appCfg.LLMTimeout)
		defer cancel()

		// 组装 user message：chat log + @者 + prompt + extra
		userMsg := "以下是群聊记录：\n" + chatLog + "\n"
		userMsg += "当前@你的是群友 " + mentionerNick + "。\n"
		if extra != "" {
			userMsg += "@你的人补充说这些,优先级较高:" + extra + "。\n"
		}
		userMsg += keywordPrompt + "\n"

		reply, err := r.llmClient.Review(ctx, userMsg, systemPrompt)
		if err != nil {
			logutil.Error("大模型调用失败", "err", err)
			r.obClient.SendGroupMessage(groupID, "大师今天罢工了，稍后再试~")
			return nil
		}
		r.obClient.SendGroupMessage(groupID, reply)

		// 保存锚点（窗口第一条消息的 MsgID）
		cache.SetLLMAnchor(groupID, cache.LLMAnchor(chatLogMsgs[0].MsgID))
		return nil
	})
}
