package cmd

import (
	"fmt"
	"testing"

	"good-review-master/cache"
)

// makeChatMsgs 构造 n 条连续 MsgID 的消息（从 1001 起），内容固定为「消息N」。
// 每条 ChatLogTokens 恰为 7：昵称2 + 全角冒号1 + 内容3（消息2 + 数字1）+ 换行1。
func makeChatMsgs(n int) []cache.Message {
	msgs := make([]cache.Message, n)
	for i := 0; i < n; i++ {
		msgs[i] = cache.Message{
			MsgID:   int64(1001 + i),
			GroupID: "10001",
			UserID:  "1",
			Nick:    "张三",
			Content: fmt.Sprintf("消息%d", i),
		}
	}
	return msgs
}

func TestDecideChatWindow(t *testing.T) {
	tests := []struct {
		name             string
		msgs             []cache.Message
		anchor           *cache.LLMAnchor
		cacheHitCost     float64
		cacheMissCost    float64
		maxContextTokens int
		llmSendCount     int
		systemTokens     int
		wantMode         string
		wantReason       string // 空串表示不校验（扩展路径不关心 ResetReason）
		wantFirstMsgID   int64  // 期望窗口首条 MsgID
		wantLen          int    // 期望窗口条数
	}{
		{
			name:             "无锚点走重置：只发最近 llmSendCount 条",
			msgs:             makeChatMsgs(5),
			anchor:           nil,
			cacheHitCost:     0.033,
			cacheMissCost:    1.0,
			maxContextTokens: 50000,
			llmSendCount:     3,
			systemTokens:     100,
			wantMode:         "reset",
			wantReason:       "锚点丢失",
			wantFirstMsgID:   1003,
			wantLen:          3,
		},
		{
			name:             "锚点命中且扩展更便宜：窗口 = 旧窗口 + 新消息",
			msgs:             makeChatMsgs(7),
			anchor:           &cache.LLMAnchor{Start: 1003, LastSent: 1005},
			cacheHitCost:     0.033,
			cacheMissCost:    1.0,
			maxContextTokens: 50000,
			llmSendCount:     3,
			systemTokens:     100,
			wantMode:         "extend",
			wantFirstMsgID:   1003,
			wantLen:          5,
		},
		{
			name:             "命中价极高时扩展贵于重置：走重置",
			msgs:             makeChatMsgs(7),
			anchor:           &cache.LLMAnchor{Start: 1003, LastSent: 1005},
			cacheHitCost:     1000, // 命中价抬高 → 扩展成本远大于重置
			cacheMissCost:    1.0,
			maxContextTokens: 50000,
			llmSendCount:     3,
			systemTokens:     100,
			wantMode:         "reset",
			wantReason:       "成本不优",
			wantFirstMsgID:   1005,
			wantLen:          3,
		},
		{
			name:             "上下文护栏超限：强制重置且窗口截断到护栏内",
			msgs:             makeChatMsgs(7),
			anchor:           &cache.LLMAnchor{Start: 1003, LastSent: 1005},
			cacheHitCost:     0.033,
			cacheMissCost:    1.0,
			maxContextTokens: 110, // 系统100 + 每条7：3条=121>110，2条=114>110，1条=107<=110
			llmSendCount:     3,
			systemTokens:     100,
			wantMode:         "reset",
			wantReason:       "成本不优",
			wantFirstMsgID:   1007,
			wantLen:          1,
		},
		{
			name:             "锚点 Start 已不在缓存：走重置",
			msgs:             makeChatMsgs(5),
			anchor:           &cache.LLMAnchor{Start: 9999, LastSent: 9999},
			cacheHitCost:     0.033,
			cacheMissCost:    1.0,
			maxContextTokens: 50000,
			llmSendCount:     3,
			systemTokens:     100,
			wantMode:         "reset",
			wantReason:       "锚点丢失",
			wantFirstMsgID:   1003,
			wantLen:          3,
		},
		{
			name:             "LastSent 缺失但 Start 在：lastSent 回退到锚点自身，仍走扩展",
			msgs:             makeChatMsgs(7),
			anchor:           &cache.LLMAnchor{Start: 1003, LastSent: 99999},
			cacheHitCost:     0.033,
			cacheMissCost:    1.0,
			maxContextTokens: 50000,
			llmSendCount:     3,
			systemTokens:     100,
			wantMode:         "extend",
			wantFirstMsgID:   1003,
			wantLen:          5,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := decideChatWindow(tc.msgs, tc.anchor, tc.cacheHitCost, tc.cacheMissCost,
				tc.maxContextTokens, tc.llmSendCount, tc.systemTokens)

			if got.Mode != tc.wantMode {
				t.Fatalf("Mode = %q，期望 %q", got.Mode, tc.wantMode)
			}
			if tc.wantReason != "" && got.ResetReason != tc.wantReason {
				t.Fatalf("ResetReason = %q，期望 %q", got.ResetReason, tc.wantReason)
			}
			if len(got.Window) == 0 {
				t.Fatal("Window 为空")
			}
			if got.Window[0].MsgID != tc.wantFirstMsgID {
				t.Fatalf("窗口首条 MsgID = %d，期望 %d", got.Window[0].MsgID, tc.wantFirstMsgID)
			}
			if len(got.Window) != tc.wantLen {
				t.Fatalf("窗口条数 = %d，期望 %d", len(got.Window), tc.wantLen)
			}
		})
	}
}

// TestDecideChatWindow_ResetWindowWithinGuard 验证护栏截断的不变量：
// 重置窗口的 token（系统提示 + 窗口）不超过上限，除非只剩 1 条也压不下来。
func TestDecideChatWindow_ResetWindowWithinGuard(t *testing.T) {
	combos := []struct {
		maxContextTokens int
		systemTokens     int
		llmSendCount     int
	}{
		{110, 100, 3},   // 普通截断
		{50, 100, 3},    // 系统提示本身就超上限 → 截到只剩 1 条
		{500, 200, 10},  // 护栏宽松，窗口不截断
	}
	for _, c := range combos {
		d := decideChatWindow(makeChatMsgs(7), nil, 0.033, 1.0, c.maxContextTokens, c.llmSendCount, c.systemTokens)
		if d.Mode != "reset" {
			t.Fatalf("组合 %+v：Mode = %q，期望 reset", c, d.Mode)
		}
		actualTokens := c.systemTokens + cache.ChatLogTokens(d.Window)
		if len(d.Window) > 1 && actualTokens > c.maxContextTokens {
			t.Fatalf("组合 %+v：截断后窗口 %d 条 token=%d 仍超上限 %d",
				c, len(d.Window), actualTokens, c.maxContextTokens)
		}
	}
}
