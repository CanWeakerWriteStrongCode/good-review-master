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
// 决策逻辑在 decideChatWindow（cmd/chat_window.go，纯函数，可单测穷举）；这里只做
// 配置/锚点读取 + 日志输出。扩展条件：锚点可用、扩展成本 < 重置成本、未超上下文护栏。
func (r *Router) selectChatWindow(msgs []cache.Message, groupID string, systemPrompt string) []cache.Message {
	// systemTokens = 固定前缀 P（systemPrompt + 常量前缀）的 token 数：扩展/重置都会发送
	systemTokens := cache.EstimateTokens(systemPrompt) + cache.EstimateTokens(userLogPrefix)
	decision := decideChatWindow(msgs, cache.GetLLMAnchor(groupID),
		r.appCfg.LLMConfig.CacheHitCost, r.appCfg.LLMConfig.CacheMissCost,
		r.appCfg.LLMConfig.MaxContextTokens, r.appCfg.LLMSendCount, systemTokens)

	if decision.Mode == "extend" {
		logutil.Debug("缓存扩展", "group", groupID, "窗口", len(decision.Window),
			"命中token", decision.HitTokens, "新增token", decision.NewTokens, "重置token", decision.ResetTokens,
			"扩展成本", decision.ExtendCost, "重置成本", decision.ResetCost)
	} else {
		logutil.Debug("缓存重置("+decision.ResetReason+")", "group", groupID, "窗口", len(decision.Window),
			"重置token", decision.ResetTokens)
	}
	return decision.Window
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
