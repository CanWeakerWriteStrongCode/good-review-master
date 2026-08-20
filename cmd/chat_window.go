package cmd

import (
	"good-review-master/cache"
)

// windowDecision 选窗决策结果：要发送的窗口 + 决策信息（供日志与单元测试断言）。
type windowDecision struct {
	Window      []cache.Message // 本次要发送的窗口（扩展 = msgs[anchorIndex:]，重置 = 最近 llmSendCount 条并护栏截断）
	Mode        string          // "extend"（缓存扩展）或 "reset"（重置）
	ResetReason string          // 仅 reset 时：锚点丢失 / 成本不优
	HitTokens   int             // 扩展路径命中前缀 token（系统提示 + 上次已发窗口）
	NewTokens   int             // 扩展路径新增 token（上次之后到现在的消息）
	ResetTokens int             // 重置窗口 token：扩展时是成本对比用的完整重置窗口；重置时是护栏截断后的实际窗口
	ExtendCost  float64         // 扩展成本 = 命中前缀 × 命中价 + 新增 × 未命中价
	ResetCost   float64         // 重置成本 = 重置窗口 × 未命中价
}

// decideChatWindow 按 token 成本决定发送「扩展窗口（缓存命中）」还是「重置窗口」。
// 纯函数：所有输入显式传入，不依赖 Router/全局状态，便于表格驱动单元测试穷举各分支。
//
// 扩展条件（全部满足才扩展，否则重置）：
//  1. 锚点可用：上次发送窗口的首条（Start）仍能在 msgs 中找到；
//  2. 扩展成本 < 重置成本（命中前缀按 cacheHitCost 计，新增按 cacheMissCost 计）；
//  3. 未超上下文护栏 maxContextTokens（超限强制重置）。
//
// 重置窗口 = 最近 llmSendCount 条；若重置窗口也超护栏，则从旧到新逐条截断到护栏内。
func decideChatWindow(msgs []cache.Message, anchor *cache.LLMAnchor,
	cacheHitCost, cacheMissCost float64, maxContextTokens, llmSendCount, systemTokens int) windowDecision {

	decision := windowDecision{ResetReason: "锚点丢失"}

	if anchor != nil {
		anchorIndex := cache.FindMsgIndex(msgs, anchor.Start)
		// 防御：LastSent 找不到时按仅锚点自身计（命中前缀取最小）
		lastSentIndex := max(cache.FindMsgIndex(msgs, anchor.LastSent), anchorIndex)
		if anchorIndex >= 0 {
			// 命中前缀 = 上次发送过的完整窗口；新增 = 上次之后到现在的消息
			previousWindowTokens := cache.ChatLogTokens(msgs[anchorIndex : lastSentIndex+1])
			decision.NewTokens = cache.ChatLogTokens(msgs[lastSentIndex+1:])
			decision.HitTokens = systemTokens + previousWindowTokens

			// 重置窗口 token（用于和扩展成本对比）
			resetWindowStart := max(len(msgs)-llmSendCount, 0)
			decision.ResetTokens = systemTokens + cache.ChatLogTokens(msgs[resetWindowStart:])

			decision.ExtendCost = float64(decision.HitTokens)*cacheHitCost + float64(decision.NewTokens)*cacheMissCost
			decision.ResetCost = float64(decision.ResetTokens) * cacheMissCost
			if decision.ExtendCost < decision.ResetCost && (maxContextTokens <= 0 || decision.HitTokens+decision.NewTokens <= maxContextTokens) {
				decision.Window = msgs[anchorIndex:]
				decision.Mode = "extend"
			} else {
				decision.ResetReason = "成本不优"
			}
		}
	}

	if decision.Mode != "extend" {
		// 重置：发最近 llmSendCount 条，锚点丢失时也是走这里
		resetStart := max(len(msgs)-llmSendCount, 0)
		decision.Window = msgs[resetStart:]
		decision.Mode = "reset"
		// 防御：重置窗口也截断到上下文护栏内
		totalTokens := systemTokens + cache.ChatLogTokens(decision.Window)
		for maxContextTokens > 0 && totalTokens > maxContextTokens && len(decision.Window) > 1 {
			totalTokens -= cache.ChatLogTokens(decision.Window[:1])
			decision.Window = decision.Window[1:]
		}
		decision.ResetTokens = systemTokens + cache.ChatLogTokens(decision.Window)
	}

	return decision
}
