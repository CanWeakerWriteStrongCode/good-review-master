package router

import (
	"strings"
	"testing"

	"good-review-master/config"
)

func TestComposeSystemPrompt包含稳定身份与at约定(t *testing.T) {
	cfg := &config.Config{BotNickname: "不是好评大师", LLMConfig: config.LLMConf{ModelName: "m"}}

	prompt := ComposeSystemPrompt(cfg, "0C2F9A2A", "")
	if !strings.Contains(prompt, "你的昵称是【不是好评大师】，用户ID是【0C2F9A2A】") {
		t.Fatalf("提示词应同时给出昵称与稳定 ID：%s", prompt)
	}
	if !strings.Contains(prompt, "user_id 是发言者的稳定ID") || !strings.Contains(prompt, "@昵称(ID)") {
		t.Fatalf("提示词应说明 @昵称(ID) 与 user_id 的约定：%s", prompt)
	}
	if !strings.Contains(prompt, "message_id 是本窗口内的消息序号") || !strings.Contains(prompt, "nickname 是昵称") {
		t.Fatalf("提示词应说明与 OneBot 对齐的字段名：%s", prompt)
	}
	if !strings.Contains(prompt, "reply_id") {
		t.Fatalf("提示词应说明 reply_id 的含义：%s", prompt)
	}
}

func TestStripCQPrefix剥离各种at写法(t *testing.T) {
	cfg := &config.Config{BotNickname: "不是好评大师"}
	instance := &Router{}

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "昵称加 ID", raw: "@不是好评大师(0C2F9A2A) 锐评", want: "锐评"},
		{name: "多个 at 且机器人不在第一个", raw: "@张三(3319D7) @不是好评大师(0C2F9A2A) 锐评", want: "锐评"},
		{name: "手打昵称", raw: "@不是好评大师 锐评", want: "锐评"},
		{name: "保留正文里的其他 at", raw: "@不是好评大师(0C2F9A2A) 告诉@张三(3319D7)", want: "告诉@张三(3319D7)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := instance.stripCQPrefix(tc.raw, cfg); got != tc.want {
				t.Fatalf("stripCQPrefix() = %q, want %q", got, tc.want)
			}
		})
	}
}
