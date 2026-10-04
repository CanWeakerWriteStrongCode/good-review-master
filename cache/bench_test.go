package cache

import (
	"strconv"
	"testing"
)

// BenchmarkAdd 验证 cache 的核心声称：**零拷贝**。
//
// 判据是 -benchmem 里的 allocs/op 必须为 0。环形缓冲的全部意义就在于
// "只分配一次、之后每写一条消息都不再分配"，一旦有人往 Add 的路径上塞了
// 会逃逸到堆的东西（哪怕只是一句埋点），这个数字立刻就会变成 1，
// 而那正是这条基准存在的理由——它是那个声称的可执行版本。
//
// 跑法：go test -bench BenchmarkAdd -benchmem ./cache/
func BenchmarkAdd(b *testing.B) {
	gc := &GroupMsgCache{
		buf:      make([]Message, 1024),
		msgIDSet: make(map[int64]struct{}, 1024),
	}
	msg := Message{MsgID: 1, GroupID: "1", UserID: "1", Nick: "张三", Content: "今天好累"}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		msg.MsgID = int64(i + 1)
		gc.Add(msg)
	}
}

// BenchmarkGetAll 量的是"取一次完整快照"的代价：每次都要按时间顺序复制一份，
// 所以这里**应该**有分配（一次 make + 一次 copy），不是 0。
// 记下来是为了让"n≈20 条的复制可以忽略"这句话有个具体数字。
func BenchmarkGetAll(b *testing.B) {
	gc := &GroupMsgCache{
		buf:      make([]Message, 20),
		msgIDSet: make(map[int64]struct{}, 20),
	}
	for i := 0; i < 20; i++ {
		gc.Add(Message{MsgID: int64(i + 1), Content: "消息" + strconv.Itoa(i)})
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = gc.GetAll()
	}
}

// BenchmarkHasMsgID 去重是轮询热路径上每条消息都要做的 O(1) 查找，必须零分配。
func BenchmarkHasMsgID(b *testing.B) {
	gc := &GroupMsgCache{
		buf:      make([]Message, 1024),
		msgIDSet: make(map[int64]struct{}, 1024),
	}
	for i := 0; i < 512; i++ {
		gc.Add(Message{MsgID: int64(i + 1)})
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = gc.HasMsgID(256)
	}
}

// BenchmarkBuildChatLog 组装发给大模型的上下文。它不是热点（每次锐评一次），
// 但它的输出**长度**直接决定 token 花费，所以这里有数字比没有好。
func BenchmarkBuildChatLog(b *testing.B) {
	msgs := make([]Message, 20)
	for i := range msgs {
		msgs[i] = Message{
			MsgID:   int64(i + 1),
			UserID:  "123456",
			Nick:    "张三",
			Content: "今天好累啊，谁来陪我说说话",
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = BuildChatLog(msgs)
	}
}

// BenchmarkEstimateTokens 选窗决策会对着候选窗口反复估算 token，
// 它是每次锐评里被调用次数最多的函数之一。
func BenchmarkEstimateTokens(b *testing.B) {
	text := "今天好累啊，谁来陪我说说话。这是一条长度接近真实群消息的样本内容。"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = EstimateTokens(text)
	}
}
