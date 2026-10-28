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

// 同一个群被两个机器人账号同时收消息时，同一条消息只保留一份；
// 但同一个账号自己的 message_id 去重要单独成立。
func Test跨账号重复消息只存一份(t *testing.T) {
	gc := &GroupMsgCache{
		buf:     make([]Message, 8),
		seenSet: make(map[seenKey]struct{}, 8),
	}
	base := Message{
		GroupID:  "G",
		UserID:   "U",
		Nickname: "张三",
		Content:  "今天好累",
		Time:     1700000000,
	}
	first := base
	first.SelfID = "bot-a"
	first.MessageID = 1
	second := base
	second.SelfID = "bot-b"
	second.MessageID = 2
	gc.Add(first)
	gc.Add(second)

	if got := gc.Len(); got != 1 {
		t.Fatalf("同一消息被两个账号收到，应只缓存一条，得到 %d", got)
	}
	if !gc.HasSeen("bot-a", 1) {
		t.Fatal("bot-a 的消息应标记为已处理")
	}
	if gc.HasSeen("bot-b", 2) {
		t.Fatal("被跨账号去重掉的消息不应算 bot-b 已处理")
	}
	if gc.HasSeen("bot-a", 2) {
		t.Fatal("seen 必须按 (self_id, message_id) 区分")
	}
}
