package onebot

import (
	"strings"
	"testing"
)

func TestRenderText(t *testing.T) {
	event := Event{
		Message: []Segment{
			ReplySegment("9"),
			MentionSegment("u1"),
			TextSegment(" 看这个 "),
			ImageSegment("https://example.com/a.png"),
			{Type: "face"},
		},
	}
	got := event.RenderText(func(userID string) string {
		if userID == "u1" {
			return "张三"
		}
		return ""
	})
	for _, want := range []string{"@张三(u1)", "看这个", "[图片]", "[face]"} {
		if !strings.Contains(got, want) {
			t.Errorf("RenderText = %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "(9)") {
		t.Errorf("reply 段不应进入正文：%q", got)
	}
}

func TestRenderText不知道昵称时用ID占位(t *testing.T) {
	event := Event{Message: []Segment{MentionSegment("u1")}}
	if got := event.RenderText(nil); got != "@用户(u1)" {
		t.Fatalf("got %q", got)
	}
}

func TestRenderText空内容回退AltMessage(t *testing.T) {
	event := Event{AltMessage: "fallback", Message: []Segment{{Type: "unknown"}}}
	if got := event.RenderText(nil); got != "[unknown]" {
		t.Fatalf("got %q", got)
	}
	if got := (Event{AltMessage: "fallback"}).RenderText(nil); got != "fallback" {
		t.Fatalf("空消息应回退 alt_message，got %q", got)
	}
}

func TestMentionsBot(t *testing.T) {
	base := Event{
		Self:    Self{Platform: "qq", UserID: "bot-openid"},
		Message: []Segment{TextSegment("hi"), MentionSegment("bot-openid")},
	}
	if !base.MentionsBot() {
		t.Fatal("mention.user_id == self.user_id 应触发")
	}

	other := base
	other.Self.UserID = "another"
	if other.MentionsBot() {
		t.Fatal("mention 别人不应触发")
	}

	sentinel := base
	sentinel.Self.UserID = SentinelSelfID
	if sentinel.MentionsBot() || sentinel.BotID() != "" {
		t.Fatal("哨兵 self.user_id 不可比较，必须不触发")
	}

	all := Event{Self: Self{UserID: "bot"}, Message: []Segment{{Type: SegmentMentionAll}}}
	if all.MentionsBot() || !all.MentionsAll() {
		t.Fatal("@全体不等于 @ 本账号")
	}
}

func TestQuotedMessageID和Images(t *testing.T) {
	event := Event{Message: []Segment{
		ReplySegment("42"),
		ImageSegment("https://example.com/a.png"),
		{Type: SegmentImage, Data: map[string]any{"url": "https://example.com/b.png"}},
		TextSegment("hi"),
	}}
	if got := event.QuotedMessageID(); got != "42" {
		t.Fatalf("QuotedMessageID = %q", got)
	}
	images := event.Images()
	if len(images) != 2 || images[0] != "https://example.com/a.png" {
		t.Fatalf("Images = %v", images)
	}
}

func TestPlainText只取文本段(t *testing.T) {
	event := Event{
		Message: []Segment{MentionSegment("u1"), TextSegment("#帮助")},
	}
	if got := event.PlainText(); got != "#帮助" {
		t.Fatalf("PlainText = %q", got)
	}
}
