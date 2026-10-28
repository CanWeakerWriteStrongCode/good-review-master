package cache

import (
	"strings"
	"testing"
)

func TestBuildChatLog保留字符串UserID(t *testing.T) {
	log := BuildChatLog([]Message{{
		MsgID:   1,
		UserID:  "79EBED44D996F294D8548AA8B0767D4C",
		Nick:    "薛定谔的价值",
		Content: "@用户(3319D76763F54481B6E5C25D68A787FA) test",
		QuoteID: 42,
	}})

	if !strings.Contains(log, `"user_id":"79EBED44D996F294D8548AA8B0767D4C"`) {
		t.Fatalf("openid 应原样保留在 user_id 中：%s", log)
	}
	if !strings.Contains(log, `"content":"@用户(3319D76763F54481B6E5C25D68A787FA) test"`) {
		t.Fatalf("content 应保留 @昵称(ID) 形态：%s", log)
	}
	if !strings.Contains(log, `"quote_id":42`) {
		t.Fatalf("引用关系应作为 quote_id 出现在聊天记录里：%s", log)
	}
}
