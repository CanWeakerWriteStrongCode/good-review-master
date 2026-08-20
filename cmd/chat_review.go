package cmd

import (
	"context"
	"good-review-master/cache"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

// userLogPrefix 发送给 LLM 的聊天记录前缀（成本模型中的常量前缀 P 的一部分）
const userLogPrefix = "以下是群聊记录：\n"

// chatReview 异步锐评（通过 async 管理生命周期，自动继承 shutdown context）
func (r *Router) chatReview(event onebot.Event, groupID string, systemPrompt string, keywordPrompt string, mentionerNick string, extra string) {
	logutil.Info("触发锐评", "group", groupID, "user", event.Nickname)
	r.Go(func(ctx context.Context) error {
		msgs := cache.GetGroupCache(groupID, r.appCfg.MaxCacheMsg).GetAll()
		if len(msgs) == 0 {
			r.obClient.SendGroupMessage(groupID, "暂无群聊记录，无法锐评~")
			return nil
		}

		// 按 token 成本决定扩展/重置，得到本次要发送的窗口
		chatLogMsgs := r.selectChatWindow(msgs, groupID, systemPrompt)
		chatLog := cache.BuildChatLog(chatLogMsgs)

		ctx, cancel := context.WithTimeout(ctx, r.appCfg.LLMTimeout)
		defer cancel()
		userMsg := buildUserMsg(chatLog, mentionerNick, keywordPrompt, extra)

		reply, err := r.llmClient.Review(ctx, userMsg, systemPrompt)
		if err != nil {
			logutil.Error("大模型调用失败", "err", err)
			r.obClient.SendGroupMessage(groupID, "大师今天罢工了，稍后再试~")
			return nil
		}
		r.obClient.SendGroupMessage(groupID, reply)

		// 保存锚点（窗口首条 + 末条 MsgID）
		cache.SetLLMAnchor(groupID, cache.LLMAnchor{
			Start:    chatLogMsgs[0].MsgID,
			LastSent: chatLogMsgs[len(chatLogMsgs)-1].MsgID,
		})
		return nil
	})
}

// selectChatWindow 按 token 成本决定扩展还是重置，返回本次要发送的窗口消息。
// 扩展条件：锚点可用（上次发送的首尾消息仍能找到），且扩展成本 < 重置成本，
// 且命中前缀+新增不超过上下文护栏 max_context_tokens（超限强制重置）。
func (r *Router) selectChatWindow(msgs []cache.Message, groupID string, systemPrompt string) []cache.Message {
	cacheHitCost := r.appCfg.LLMConfig.CacheHitCost
	cacheMissCost := r.appCfg.LLMConfig.CacheMissCost
	maxContextTokens := r.appCfg.LLMConfig.MaxContextTokens
	llmSendCount := r.appCfg.LLMSendCount
	// systemTokens = 固定前缀 P（systemPrompt + 常量前缀）的 token 数：扩展/重置都会发送
	systemTokens := cache.EstimateTokens(systemPrompt) + cache.EstimateTokens(userLogPrefix)

	var chatLogMsgs []cache.Message
	resetReason := "锚点丢失"
	anchor := cache.GetLLMAnchor(groupID)
	if anchor != nil {
		anchorIndex := cache.FindMsgIndex(msgs, anchor.Start)
		// 防御：LastSent 找不到时按仅锚点自身计（命中前缀取最小）
		lastSentIndex := max(cache.FindMsgIndex(msgs, anchor.LastSent), anchorIndex)
		if anchorIndex >= 0 {
			// 命中前缀 = 上次发送过的完整窗口；新增 = 上次之后到现在的消息
			previousWindowTokens := cache.ChatLogTokens(msgs[anchorIndex : lastSentIndex+1])
			newTokens := cache.ChatLogTokens(msgs[lastSentIndex+1:])
			hitTokens := systemTokens + previousWindowTokens

			// 重置窗口 token（用于和扩展成本对比）
			resetWindowStart := max(len(msgs)-llmSendCount, 0)
			resetTokens := systemTokens + cache.ChatLogTokens(msgs[resetWindowStart:])

			extendCost := float64(hitTokens)*cacheHitCost + float64(newTokens)*cacheMissCost
			resetCost := float64(resetTokens) * cacheMissCost
			if extendCost < resetCost && (maxContextTokens <= 0 || hitTokens+newTokens <= maxContextTokens) {
				chatLogMsgs = msgs[anchorIndex:]
				logutil.Debug("缓存扩展", "group", groupID, "窗口", len(chatLogMsgs),
					"命中token", hitTokens, "新增token", newTokens, "重置token", resetTokens,
					"扩展成本", extendCost, "重置成本", resetCost)
			} else {
				resetReason = "成本不优"
			}
		}
	}
	if len(chatLogMsgs) == 0 {
		// 重置：发最近 llm_send_count 条，锚点丢失时也是走这里
		resetStart := max(len(msgs)-llmSendCount, 0)
		chatLogMsgs = msgs[resetStart:]
		// 防御：重置窗口也截断到上下文护栏内
		totalTokens := systemTokens + cache.ChatLogTokens(chatLogMsgs)
		for maxContextTokens > 0 && totalTokens > maxContextTokens && len(chatLogMsgs) > 1 {
			totalTokens -= cache.ChatLogTokens(chatLogMsgs[:1])
			chatLogMsgs = chatLogMsgs[1:]
		}
		logutil.Debug("缓存重置("+resetReason+")", "group", groupID, "窗口", len(chatLogMsgs),
			"重置token", systemTokens+cache.ChatLogTokens(chatLogMsgs))
	}
	return chatLogMsgs
}

// buildUserMsg 组装发给大模型的 user message：聊天记录 + @者信息 + 关键词 prompt
func buildUserMsg(chatLog string, mentionerNick string, keywordPrompt string, extra string) string {
	userMsg := userLogPrefix + chatLog + "\n"
	userMsg += "当前@你的是群友 " + mentionerNick + "。\n"
	if extra != "" {
		userMsg += "@你的人补充说这些,优先级较高:" + extra + "。\n"
	}
	userMsg += keywordPrompt + "\n"
	return userMsg
}
