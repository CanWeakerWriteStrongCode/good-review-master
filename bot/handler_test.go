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
			if got := isAtBot(tc.event, tc.content, cfg); got != tc.want {
				t.Fatalf("isAtBot() = %v, want %v", got, tc.want)
			}
		})
	}
}
