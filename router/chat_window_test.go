package router

import (
	"fmt"
	"strconv"
	"testing"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
)

// makeChatMsgs 构造 n 条连续 MsgID 的消息（从 1001 起），内容固定为「消息N」。
// 单条 JSON 行 ChatLogTokens ≈ 11：昵称2 + 内容3 + JSON 外壳与换行的 ASCII 折算（7 计）。
func makeChatMsgs(n int) []cache.Message {
	msgs := make([]cache.Message, n)
	for i := 0; i < n; i++ {
		msgs[i] = cache.Message{
			MessageID: strconv.Itoa(1001 + i),
			GroupID:   "10001",
			UserID:    "1",
			Nickname:  "张三",
			Content:   fmt.Sprintf("消息%d", i),
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
		wantFirstMsgID   string // 期望窗口首条 MsgID
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
			wantFirstMsgID:   "1003",
			wantLen:          3,
		},
		{
			name:             "锚点命中且扩展更便宜：窗口 = 旧窗口 + 新消息",
			msgs:             makeChatMsgs(7),
			anchor:           &cache.LLMAnchor{Start: "1003", LastSent: "1005"},
			cacheHitCost:     0.033,
			cacheMissCost:    1.0,
			maxContextTokens: 50000,
			llmSendCount:     3,
			systemTokens:     100,
			wantMode:         "extend",
			wantFirstMsgID:   "1003",
			wantLen:          5,
		},
		{
			name:             "命中价极高时扩展贵于重置：走重置",
			msgs:             makeChatMsgs(7),
			anchor:           &cache.LLMAnchor{Start: "1003", LastSent: "1005"},
			cacheHitCost:     1000, // 命中价抬高 → 扩展成本远大于重置
			cacheMissCost:    1.0,
			maxContextTokens: 50000,
			llmSendCount:     3,
			systemTokens:     100,
			wantMode:         "reset",
			wantReason:       "成本不优",
			wantFirstMsgID:   "1005",
			wantLen:          3,
		},
		{
			name:             "上下文护栏超限：强制重置且窗口截断到护栏内",
			msgs:             makeChatMsgs(7),
			anchor:           &cache.LLMAnchor{Start: "1003", LastSent: "1005"},
			cacheHitCost:     0.033,
			cacheMissCost:    1.0,
			maxContextTokens: 115, // 系统100 + 单条约11：重置窗口(3条)超限，2条也>115，截到1条(111<=115)
			llmSendCount:     3,
			systemTokens:     100,
			wantMode:         "reset",
			wantReason:       "成本不优",
			wantFirstMsgID:   "1007",
			wantLen:          1,
		},
		{
			name:             "锚点 Start 已不在缓存：走重置",
			msgs:             makeChatMsgs(5),
			anchor:           &cache.LLMAnchor{Start: "9999", LastSent: "9999"},
			cacheHitCost:     0.033,
			cacheMissCost:    1.0,
			maxContextTokens: 50000,
			llmSendCount:     3,
			systemTokens:     100,
			wantMode:         "reset",
			wantReason:       "锚点丢失",
			wantFirstMsgID:   "1003",
			wantLen:          3,
		},
		{
			name:             "LastSent 缺失但 Start 在：lastSent 回退到锚点自身，仍走扩展",
			msgs:             makeChatMsgs(7),
			anchor:           &cache.LLMAnchor{Start: "1003", LastSent: "99999"},
			cacheHitCost:     0.033,
			cacheMissCost:    1.0,
			maxContextTokens: 50000,
			llmSendCount:     3,
			systemTokens:     100,
			wantMode:         "extend",
			wantFirstMsgID:   "1003",
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
			if got.Window[0].MessageID != tc.wantFirstMsgID {
				t.Fatalf("窗口首条 MessageID = %s，期望 %s", got.Window[0].MessageID, tc.wantFirstMsgID)
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
		{110, 100, 3},  // 普通截断
		{50, 100, 3},   // 系统提示本身就超上限 → 截到只剩 1 条
		{500, 200, 10}, // 护栏宽松，窗口不截断
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

// TestSelectChatWindow_AnchorExpired TTL 判断发生在 selectChatWindow 里、把锚点交给
// decideChatWindow 之前：过期就当没有锚点，直接退回重置窗口。
//
// 为什么要在决策之前拦：decideChatWindow 拿到的锚点若是失效的，算出的扩展成本是假账
// （前缀按命中价 0.033 计），而真实情况是这段 KV 没了、几万 token 全额重算，比重置贵。
func TestSelectChatWindow_AnchorExpired(t *testing.T) {
	setupTestLogger(t)

	const groupID = "ttl-test-group"
	// 1001..1030。锚点 Start=1001 说明前 29 条都发过，只差最后 1 条 → 扩展一定划算；
	// 重置窗口取最近 llm_send_count=20 条，也就是 1011..1030。
	msgs := makeChatMsgs(30)

	newRouter := func(ttl time.Duration) *Router {
		return &Router{
			appCfg: staticSnapshot(&config.Config{
				LLMSendCount: 20,
				CacheTTL:     ttl,
				LLMConfig: config.LLMConf{
					CacheHitCost:     0.033,
					CacheMissCost:    1.0,
					MaxContextTokens: 50000,
				},
			}),
		}
	}

	tests := []struct {
		name      string
		ttl       time.Duration
		anchorAt  time.Time
		wantFirst string
		wantLen   int
	}{
		{
			name: "锚点还新鲜：扩展，保留更早的历史", ttl: 15 * time.Minute,
			anchorAt: time.Now(), wantFirst: "1001", wantLen: 30,
		},
		{
			name: "锚点过期：按无锚点处理，退回最近 llm_send_count 条", ttl: 15 * time.Minute,
			anchorAt: time.Now().Add(-time.Hour), wantFirst: "1011", wantLen: 20,
		},
		{
			name: "ttl 配 0：不判过期，隔一天也照样扩展", ttl: 0,
			anchorAt: time.Now().Add(-24 * time.Hour), wantFirst: "1001", wantLen: 30,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache.ResetAll()
			cache.SetLLMAnchor(groupID, cache.LLMAnchor{Start: "1001", LastSent: "1029", At: tt.anchorAt})
			r := newRouter(tt.ttl)
			got := r.selectChatWindow(msgs, groupID, "", r.appCfg())
			if len(got) != tt.wantLen || got[0].MessageID != tt.wantFirst {
				t.Fatalf("窗口 = %d 条，首条 %s；期望 %d 条，首条 %s",
					len(got), got[0].MessageID, tt.wantLen, tt.wantFirst)
			}
		})
	}
}
