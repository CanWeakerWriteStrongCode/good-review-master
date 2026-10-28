package bot

import (
	"testing"

	"good-review-master/config"
)

// isAtBot 只处理"适配器没有产出 mention 段"的兜底：
// 标准触发由 event.MentionsBot() 判定（mention.user_id == self.user_id）。
func TestIsAtBot昵称兜底(t *testing.T) {
	cfg := &config.Config{BotNickname: "不是好评大师"}

	cases := []struct {
		name          string
		content       string
		allowNickname bool
		want          bool
	}{
		{name: "手打昵称且昵称唯一", content: "@不是好评大师 锐评", allowNickname: true, want: true},
		{name: "手打昵称但多账号不启用兜底", content: "@不是好评大师 锐评", allowNickname: false, want: false},
		{name: "别人被 at 不算", content: "@用户(3319D7) test", allowNickname: true, want: false},
		{name: "通用的 @你 仍可识别", content: "@你 锐评", allowNickname: false, want: true},
		{name: "普通文本", content: "今天天气不错", allowNickname: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAtBot(tc.content, cfg, tc.allowNickname); got != tc.want {
				t.Fatalf("isAtBot() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsAtBot空昵称不误判(t *testing.T) {
	cfg := &config.Config{}
	if isAtBot("@ 锐评", cfg, true) {
		t.Fatal("昵称为空时不能把任意 @ 当成 @ 本账号")
	}
}

func TestTruncate按字符截断(t *testing.T) {
	text := "今天好累啊"
	if got := truncate(text, 3); got != "今天好" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncate(text, 0); got != text {
		t.Fatalf("limit<=0 不截断，got %q", got)
	}
	if got := truncate(text, 100); got != text {
		t.Fatalf("未超限不截断，got %q", got)
	}
}
