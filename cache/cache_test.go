package cache

import (
	"strings"
	"testing"
)

func TestBuildChatLog保留字符串UserID(t *testing.T) {
	log := BuildChatLog([]Message{{
		MessageID: 1,
		UserID:    "79EBED44D996F294D8548AA8B0767D4C",
		Nickname:  "薛定谔的价值",
		Content:   "@用户(3319D76763F54481B6E5C25D68A787FA) test",
		ReplyToID: 42,
	}})

	if !strings.Contains(log, `"message_id":1`) {
		t.Fatalf("消息 ID 应以 OneBot 同名字段 message_id 出现：%s", log)
	}
	if !strings.Contains(log, `"user_id":"79EBED44D996F294D8548AA8B0767D4C"`) {
		t.Fatalf("openid 应原样保留在 user_id 中：%s", log)
	}
	if !strings.Contains(log, `"nickname":"薛定谔的价值"`) {
		t.Fatalf("昵称应以 nickname 字段出现：%s", log)
	}
	if !strings.Contains(log, `"message":"@用户(3319D76763F54481B6E5C25D68A787FA) test"`) {
		t.Fatalf("message 应保留 @昵称(ID) 形态：%s", log)
	}
	if !strings.Contains(log, `"reply_id":42`) {
		t.Fatalf("引用关系应作为 reply_id 出现在聊天记录里：%s", log)
	}
}
