package onebot

import "testing"

func TestNormalizeMentions统一成昵称加ID(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		selfID   string
		selfName string
		want     string
	}{
		{
			name:     "CQ at 机器人",
			raw:      "[CQ:at,qq=bot123] 你好",
			selfID:   "bot123",
			selfName: "机器人",
			want:     "@机器人(bot123) 你好",
		},
		{
			name:   "小于号 at 其他人",
			raw:    "<@3319D76763F54481B6E5C25D68A787FA> test",
			selfID: "bot123",
			want:   "@用户(3319D76763F54481B6E5C25D68A787FA) test",
		},
		{
			name:   "带感叹号的 at",
			raw:    "<@!3319D7> test",
			selfID: "bot123",
			want:   "@用户(3319D7) test",
		},
		{
			name:     "qqbot 标记",
			raw:      `<qqbot-at-user id="bot123"/> 在吗`,
			selfID:   "bot123",
			selfName: "机器人",
			want:     "@机器人(bot123) 在吗",
		},
		{
			name:   "at 所有人",
			raw:    "[CQ:at,qq=all] 全体",
			selfID: "bot123",
			want:   "@所有人 全体",
		},
		{
			name: "拿不到机器人 ID 时按普通用户处理",
			raw:  "[CQ:at,qq=bot123] 你好",
			want: "@用户(bot123) 你好",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NormalizeMentions(tc.raw, tc.selfID, tc.selfName); got != tc.want {
				t.Fatalf("NormalizeMentions() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEventBotID忽略占位值(t *testing.T) {
	cases := []struct {
		name  string
		event Event
		want  string
	}{
		{name: "self_id", event: Event{SelfID: "openid-1"}, want: "openid-1"},
		{name: "忽略 qq_official", event: Event{SelfID: "qq_official"}, want: ""},
		{name: "忽略 unknown_selfid", event: Event{SelfID: "unknown_selfid"}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.event.BotID(); got != tc.want {
				t.Fatalf("BotID() = %q, want %q", got, tc.want)
			}
		})
	}
}
