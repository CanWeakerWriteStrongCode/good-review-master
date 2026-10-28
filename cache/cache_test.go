package cache

import (
	"strings"
	"testing"
)

func TestBuildChatLog保留字符串UserID(t *testing.T) {
	log := BuildChatLog([]Message{{
		MessageID: "1",
		UserID:    "79EBED44D996F294D8548AA8B0767D4C",
		Nickname:  "薛定谔的价值",
		Content:   "@用户(3319D76763F54481B6E5C25D68A787FA) test",
		ReplyToID: "42",
	}})

	if !strings.Contains(log, `"message_id":"1"`) {
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
	if !strings.Contains(log, `"reply_id":"42"`) {
		t.Fatalf("引用关系应作为 reply_id 出现在聊天记录里：%s", log)
	}
}

// 每个工作空间一个 Store：同一个物理群在两个账号下各有一份缓存，互不影响。
func Test不同Store的群缓存互相隔离(t *testing.T) {
	a, b := NewStore(), NewStore()
	a.GetGroupCache("G", 8).Add(Message{MessageID: "1", SelfID: "bot-a", Content: "a", Time: 1})
	b.GetGroupCache("G", 8).Add(Message{MessageID: "1", SelfID: "bot-b", Content: "b", Time: 1})

	if n := a.GetGroupCache("G", 8).Len(); n != 1 {
		t.Fatalf("账号 A 缓存条数 = %d, want 1", n)
	}
	if n := b.GetGroupCache("G", 8).Len(); n != 1 {
		t.Fatalf("账号 B 缓存条数 = %d, want 1", n)
	}
	if !a.GetGroupCache("G", 8).HasSeen("bot-a", "1") || !b.GetGroupCache("G", 8).HasSeen("bot-b", "1") {
		t.Fatal("两个账号应各自记住自己的消息")
	}
}

// 同一个 Store 内 message_id 仍然去重（重连重发 / 回填与推送重叠）。
func Test同Store内消息去重(t *testing.T) {
	gc := &GroupMsgCache{
		buf:     make([]Message, 8),
		seenSet: make(map[string]struct{}, 8),
	}
	gc.Add(Message{MessageID: "1", SelfID: "bot-a", Content: "a", Time: 1})
	gc.Add(Message{MessageID: "1", SelfID: "bot-a", Content: "a", Time: 1})

	if got := gc.Len(); got != 1 {
		t.Fatalf("重复 message_id 应只缓存一条，得到 %d", got)
	}
	if !gc.HasSeen("bot-a", "1") {
		t.Fatal("消息应标记为已处理")
	}
}

func TestStats返回条数最近时间与账号(t *testing.T) {
	gc := &GroupMsgCache{
		buf:     make([]Message, 4),
		seenSet: make(map[string]struct{}, 4),
	}
	if st := gc.Stats(); st.Count != 0 || st.LastTime != 0 || st.SelfID != "" {
		t.Fatalf("空缓存应返回零值，得到 %+v", st)
	}

	gc.Add(Message{MessageID: "1", SelfID: "bot-a", Content: "a", Time: 100})
	gc.Add(Message{MessageID: "2", SelfID: "", Content: "b", Time: 101})
	gc.Add(Message{MessageID: "3", SelfID: "bot-b", Content: "c", Time: 99})

	st := gc.Stats()
	if st.Count != 3 {
		t.Fatalf("Count = %d, want 3", st.Count)
	}
	if st.LastTime != 99 {
		t.Fatalf("LastTime 应取最近一条消息的时间，得到 %d", st.LastTime)
	}
	if st.SelfID != "bot-b" {
		t.Fatalf("SelfID 应取最近一条带账号的消息，得到 %q", st.SelfID)
	}
}

func TestStats环形覆盖(t *testing.T) {
	gc := &GroupMsgCache{
		buf:     make([]Message, 2),
		seenSet: make(map[string]struct{}, 2),
	}
	gc.Add(Message{MessageID: "1", SelfID: "bot-a", Content: "a", Time: 1})
	gc.Add(Message{MessageID: "2", SelfID: "bot-a", Content: "b", Time: 2})
	gc.Add(Message{MessageID: "3", SelfID: "bot-b", Content: "c", Time: 3})

	st := gc.Stats()
	if st.Count != 2 || st.LastTime != 3 || st.SelfID != "bot-b" {
		t.Fatalf("环形覆盖后 Stats = %+v", st)
	}
}
