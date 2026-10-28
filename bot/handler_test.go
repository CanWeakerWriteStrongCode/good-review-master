package bot

import (
	"testing"

	"good-review-master/config"
	"good-review-master/onebot"
)

func TestIsAtBot使用稳定ID和昵称兜底(t *testing.T) {
	cfg := &config.Config{BotNickname: "不是好评大师"}

	cases := []struct {
		name    string
		event   onebot.Event
		content string
		want    bool
	}{
		{
			name:    "原始消息里的 openid at",
			event:   onebot.Event{SelfID: "0C2F9A2A", RawMessage: "<@0C2F9A2A> test"},
			content: "@不是好评大师(0C2F9A2A) test",
			want:    true,
		},
		{
			name:    "翻译后的 ID 命中",
			event:   onebot.Event{SelfID: "0C2F9A2A", RawMessage: "test"},
			content: "@不是好评大师(0C2F9A2A) test",
			want:    true,
		},
		{
			name:    "别人被 at 时不算",
			event:   onebot.Event{SelfID: "0C2F9A2A", RawMessage: "<@3319D7> test"},
			content: "@用户(3319D7) test",
			want:    false,
		},
		{
			name:    "手打昵称兜底",
			event:   onebot.Event{},
			content: "@不是好评大师 锐评",
			want:    true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAtBot(tc.event, tc.content, cfg, true); got != tc.want {
				t.Fatalf("isAtBot() = %v, want %v", got, tc.want)
			}
		})
	}
}

// 多账号时全局昵称不可信：@ A 的消息会以 A 的昵称开头，
// B 不能因为自己的 cfg.BotNickname 恰好是 A 的昵称就抢答。
func TestIsAtBot多账号禁用昵称兜底(t *testing.T) {
	multi := &config.Config{
		BotNickname:  "好评大师的小助理",
		AllowSelfIDs: []string{"*"},
	}
	event := onebot.Event{
		SelfID:     "0C2F9A2A",
		RawMessage: "<@FB7B890D> #切换人格 锐评",
	}
	content := "@好评大师的小助理(FB7B890D) #切换人格 锐评"
	if isAtBot(event, content, multi, false) {
		t.Fatal("多账号时 @ 另一个账号的消息不应被当成 @ 本账号")
	}

	// 同一个账号真被 @ 时仍要通过 ID 命中
	content = "@不是好评大师(0C2F9A2A) #切换人格 锐评"
	if !isAtBot(event, content, multi, false) {
		t.Fatal("命中本账号 openid 时应判定为 @")
	}

	// 单账号配置（或昵称唯一归属）仍保留手打昵称兜底
	single := &config.Config{
		BotNickname:  "不是好评大师",
		AllowSelfIDs: []string{"0C2F9A2A"},
	}
	if !isAtBot(onebot.Event{}, "@不是好评大师 锐评", single, true) {
		t.Fatal("单账号时手打昵称应继续生效")
	}
}
