package speech

import (
	"testing"

	"good-review-master/config"
	"good-review-master/onebot"
)

// 判定"这条消息是不是冲机器人来的"。从 bot 包搬过来：
// 同一个问题只该有一份实现，触发消息的调用方（agent）与分发都走这里。
//
// 标准触发看 mention.user_id 是否等于 self.user_id；昵称兜底只处理
// "适配器没有产出 mention 段"的情况，且在多账号重名时要由调用方关掉。
func TestIsAddressed判定各种点名写法(t *testing.T) {
	cfg := &config.Config{BotNickname: "不是好评大师"}
	groupEvent := func(text string, segs ...onebot.Segment) onebot.Event {
		all := append([]onebot.Segment{onebot.TextSegment(text)}, segs...)
		return onebot.Event{
			Type: "message", DetailType: "group", GroupID: "g1",
			UserID: "u1", MessageID: "m1",
			Self:    onebot.Self{Platform: "qq", UserID: "self-openid"},
			Message: all,
		}
	}

	cases := []struct {
		name          string
		event         onebot.Event
		content       string
		allowNickname bool
		want          bool
	}{
		{name: "手打昵称且昵称唯一", event: groupEvent("@不是好评大师 锐评"), content: "@不是好评大师 锐评", allowNickname: true, want: true},
		{name: "手打昵称但多账号不启用兜底", event: groupEvent("@不是好评大师 锐评"), content: "@不是好评大师 锐评", allowNickname: false, want: false},
		{name: "别人被 at 不算", event: groupEvent("@用户(3319D7) test", onebot.MentionSegment("3319D7")), content: "@用户(3319D7) test", allowNickname: true, want: false},
		{name: "标准 mention 段", event: groupEvent("锐评", onebot.MentionSegment("self-openid")), content: "锐评", allowNickname: false, want: true},
		{name: "@你 不算点名", event: groupEvent("@你 锐评"), content: "@你 锐评", allowNickname: false, want: false},
		{name: "普通文本", event: groupEvent("今天天气不错"), content: "今天天气不错", allowNickname: true, want: false},
		{name: "私聊一律算点名", event: onebot.Event{DetailType: "private", UserID: "u1"}, content: "在吗", allowNickname: false, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsAddressed(tc.event, tc.content, cfg, tc.allowNickname); got != tc.want {
				t.Fatalf("IsAddressed() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsAddressed空昵称不误判(t *testing.T) {
	cfg := &config.Config{}
	event := onebot.Event{DetailType: "group", GroupID: "g1", UserID: "u1",
		Message: []onebot.Segment{onebot.TextSegment("@ 锐评")}}
	if IsAddressed(event, "@ 锐评", cfg, true) {
		t.Fatal("昵称为空时不能把任意 @ 当成 @ 本账号")
	}
}
