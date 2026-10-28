package onebot

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestNormalizeContentMasksRichCQ(t *testing.T) {
	raw := "[CQ:reply,id=1][CQ:at,qq=2] 看这个 " +
		"[CQ:image,url=https://example.com/a.png] " +
		"[CQ:face,id=1][CQ:record,url=https://example.com/a.amr]" +
		"[CQ:video,url=https://example.com/a.mp4]" +
		"[CQ:file,name=a&#44;b.txt,url=https://example.com/f]"

	content, images := NormalizeContent(raw, 500)
	for _, want := range []string{
		"[引用]", "[图片]", "[表情]", "[语音]",
		"[视频:https://example.com/a.mp4]",
		"[文件:a,b.txt https://example.com/f]",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("content %q missing %q", content, want)
		}
	}
	if len(images) != 1 || images[0] != "https://example.com/a.png" {
		t.Errorf("images = %v", images)
	}
}

func TestEventReplyID(t *testing.T) {
	if got := (Event{MessageID: 2}).ReplyID(); got != "2" {
		t.Errorf("MessageID = %q", got)
	}
	if got := (Event{}).ReplyID(); got != "" {
		t.Errorf("empty event = %q", got)
	}
}

func TestEventQuotedMessageID(t *testing.T) {
	event := Event{
		Message: json.RawMessage(`[{"type":"reply","data":{"id":42}},{"type":"text","data":{"text":"hi"}}]`),
	}
	if got := event.QuotedMessageID(); got != 42 {
		t.Fatalf("reply 段应解析出 quote msg_id，got %d", got)
	}

	event = Event{
		Message:    json.RawMessage(`"[CQ:reply,id=7] hi"`),
		RawMessage: "[CQ:reply,id=7] hi",
	}
	if got := event.QuotedMessageID(); got != 7 {
		t.Fatalf("message 不是段数组时应退回 raw_message 的 CQ 码，got %d", got)
	}

	event = Event{RawMessage: "[CQ:reply,id=9] hi"}
	if got := event.QuotedMessageID(); got != 9 {
		t.Fatalf("raw_message CQ reply = %d", got)
	}

	if got := (Event{RawMessage: "hi"}).QuotedMessageID(); got != 0 {
		t.Fatalf("没有引用时应为 0，got %d", got)
	}
}
