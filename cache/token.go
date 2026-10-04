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
// 直接复用 BuildChatLog 的格式，成本口径与实际发给模型的内容始终一致，改格式不会两边漂移。
//
// 代价是计数时真的构建一遍 chat log 字符串（每条消息一次 json.Marshal），
// 而且调用方**传进来的往往不是"窗口"而是整个环缓存的切片**
// （见 decideChatWindow：命中前缀、新增部分、候选重置窗口各算一次）。
// 实测（router/bench_test.go）在 max_cache_msg=3100 时一次选窗约 1.9ms、6387 次分配。
//
// 这个量级对"每次锐评走一遍"完全够用，所以刻意保持现状而不是改成增量计数——
// 增量计数意味着两套口径（估算用的与实际发的格式）要各自维护，
// 改一次格式就可能悄悄漂移，而那正是这个函数要避免的事。
func ChatLogTokens(msgs []Message) int {
	return EstimateTokens(BuildChatLog(msgs))
}
