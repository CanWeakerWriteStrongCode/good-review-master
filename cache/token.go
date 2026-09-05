package cache

// EstimateTokens 启发式 token 估算：
// CJK/全角字符（rune > 0x2E7F）每字符计 1 token，其余字符每 4 字符折 1 token。
// 用于缓存扩展/重置的成本决策，不追求与真实 tokenizer 完全一致。
func EstimateTokens(s string) int {
	cjk := 0
	ascii := 0
	for _, r := range s {
		if r > 0x2E7F {
			cjk++
		} else {
			ascii++
		}
	}
	return cjk + (ascii+3)/4
}

// ChatLogTokens 按 BuildChatLog 的实际输出估算一组消息的 token 数（跳过空内容）。
// 直接复用 BuildChatLog 的格式，成本口径与实际发给模型的内容始终一致，改格式不会两边漂移；
// 代价是计数时构建一次 chat log 字符串——窗口只有几十条消息，可忽略。
func ChatLogTokens(msgs []Message) int {
	return EstimateTokens(BuildChatLog(msgs))
}
