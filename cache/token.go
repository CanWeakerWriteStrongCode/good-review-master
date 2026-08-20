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

// ChatLogTokens 按 BuildChatLog 的格式（昵称：内容\n，跳过空内容）统计一组消息的 token 数。
// 不分配字符串，避免为了计数而构建整个 chat log。
func ChatLogTokens(msgs []Message) int {
	total := 0
	for _, msg := range msgs {
		if msg.Content == "" {
			continue
		}
		total += EstimateTokens(msg.Nick) + 1    // 全角冒号：
		total += EstimateTokens(msg.Content) + 1 // 换行
	}
	return total
}
