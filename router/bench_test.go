package router

import (
	"fmt"
	"testing"

	"good-review-master/cache"
)

// buildBenchTrie 造一棵有 n 条路由的前缀树。
//
// 关键字用 "指令%04d" 是为了让它们**共享前缀**（"指令"两个字全一样），
// 这正是前缀树要对付的形状；如果关键字彼此无公共前缀，测出来的数字会好得不真实。
func buildBenchTrie(n int) *trieNode {
	root := &trieNode{children: make(map[rune]*trieNode)}
	for i := 0; i < n; i++ {
		command := &Command{Keyword: fmt.Sprintf("指令%04d", i), Category: "chat_review"}
		trieInsert(root, command.Keyword, command)
	}
	return root
}

// BenchmarkTrieMatch 检验前缀树的复杂度声称：匹配次数是 O(k)（k = 文本长度）。
//
// **实测结果比"与路由总数无关"这句注释更微妙，所以这里如实记下来**：
// 10 / 1000 / 10000 条路由分别约 24 / 42 / 51 ns——不是常数，但也远不是线性
// （路由数翻 1000 倍，耗时只翻 2 倍）。原因是 trieNode.children 是 map[rune]*trieNode：
// 共享同一个前缀的 10000 条路由会让那个节点挂上 10000 个孩子，
// 查找时的 cache miss 明显变多。算法上仍是 O(k) 次查表，涨的是每次查表的常数。
//
// 所以判据要这样用：**看它有没有随 n 线性增长**。线性增长说明有人把它改回了平铺扫描；
// 这里的两倍抬升属于内存层次效应，不是复杂度问题。
func BenchmarkTrieMatch(b *testing.B) {
	for _, count := range []int{10, 1000, 10000} {
		root := buildBenchTrie(count)
		// 命中最后插入的那条：最坏情况下要走完整条关键字
		text := fmt.Sprintf("指令%04d 帮我看看这个", count-1)

		b.Run(fmt.Sprintf("路由数=%d", count), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if trieMatch(root, text) == nil {
					b.Fatal("未命中")
				}
			}
		})
	}
}

// BenchmarkTrieMatch未命中 是另一条路径：走到第一个不匹配的字符就停，
// 比命中更快，但它同样不该随路由数变化。
func BenchmarkTrieMatch未命中(b *testing.B) {
	root := buildBenchTrie(10000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = trieMatch(root, "完全不相干的文本")
	}
}

// benchMessages 造 n 条带图片字段的正常消息（图片为空，与纯文本群一致）。
func benchMessages(n int) []cache.Message {
	msgs := make([]cache.Message, n)
	for i := range msgs {
		msgs[i] = cache.Message{
			MsgID:   int64(i + 1),
			GroupID: "1",
			UserID:  "123456",
			Nick:    "张三",
			Content: "今天好累啊，谁来陪我说说话",
			Time:    int64(1700000000 + i),
		}
	}
	return msgs
}

// BenchmarkDecideChatWindow 量选窗决策的代价。它是每次锐评都会走一遍的纯函数，
// 内部会对候选窗口反复估算 token——所以这里关注的是它**别退化成 O(n²)**，
// 而不是绝对耗时（绝对值取决于消息条数）。
func BenchmarkDecideChatWindow(b *testing.B) {
	for _, count := range []int{100, 1000, 3100} {
		msgs := benchMessages(count)
		anchor := &cache.LLMAnchor{Start: 1, LastSent: int64(count / 2)}

		b.Run(fmt.Sprintf("消息数=%d", count), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				decision := decideChatWindow(msgs, anchor, 0.033, 1.0, 50000, 20, 1000)
				if decision.Mode == "" {
					b.Fatal("决策未产出模式")
				}
			}
		})
	}
}

// BenchmarkDecideChatWindow无锚点 覆盖重置路径：锚点丢失时不需要算扩展成本，
// 但要做护栏截断，逻辑不同、代价也不同，分开量。
func BenchmarkDecideChatWindow无锚点(b *testing.B) {
	msgs := benchMessages(3100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		decision := decideChatWindow(msgs, nil, 0.033, 1.0, 50000, 20, 1000)
		if decision.Mode != "reset" {
			b.Fatalf("无锚点应走重置，实际 %s", decision.Mode)
		}
	}
}
