package cache

import (
	"strings"
	"testing"
)

func TestBuildChatLog保留字符串UserID(t *testing.T) {
	log := BuildChatLog([]Message{
		{
			RowID:     1287,
			MessageID: "ROBOT1.0_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
			UserID:    "79EBED44D996F294D8548AA8B0767D4C",
			Nickname:  "薛定谔的价值",
			Content:   "@用户(3319D76763F54481B6E5C25D68A787FA) test",
		},
		{
			RowID:     1288,
			MessageID: "ROBOT1.0_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
			UserID:    "FB7B890D38D41845542F14F25BC7A3E5",
			Nickname:  "小助理",
			Content:   "你也好",
			ReplyToID: "ROBOT1.0_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		},
		{
			RowID:     1289,
			MessageID: "ROBOT1.0_CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",
			UserID:    "u3",
			Nickname:  "路人",
			Content:   "插一句",
			ReplyToID: "ROBOT1.0_不在窗口里的消息",
		},
	})

	// 平台 ID 太长，发给大模型的是 state.db 自增主键。
	if strings.Contains(log, "ROBOT1.0_") {
		t.Fatalf("不应把平台 message_id 发给大模型：%s", log)
	}
	for _, want := range []string{`"message_id":1287`, `"message_id":1288`, `"message_id":1289`} {
		if !strings.Contains(log, want) {
			t.Fatalf("缺少行号 %s：%s", want, log)
		}
	}
	if !strings.Contains(log, `"reply_id":1287`) {
		t.Fatalf("窗口内的引用应映射成被引消息的行号：%s", log)
	}
	if strings.Count(log, `"reply_id"`) != 1 {
		t.Fatalf("引用目标不在窗口时应省略 reply_id：%s", log)
	}
	if !strings.Contains(log, `"user_id":"79EBED44D996F294D8548AA8B0767D4C"`) {
		t.Fatalf("openid 应原样保留在 user_id 中：%s", log)
	}
	if !strings.Contains(log, `"message":"@用户(3319D76763F54481B6E5C25D68A787FA) test"`) {
		t.Fatalf("message 应保留 @昵称(ID) 形态：%s", log)
	}
}

// 同一个窗口后缀换窗口发送时，同一条消息的行号必须一样——这是前缀缓存还能命中的前提。
func TestBuildChatLog行号跨窗口不变(t *testing.T) {
	msgs := []Message{
		{RowID: 501, MessageID: "m1", UserID: "u1", Nickname: "张三", Content: "第一句"},
		{RowID: 502, MessageID: "m2", UserID: "u2", Nickname: "李四", Content: "第二句"},
		{RowID: 503, MessageID: "m3", UserID: "u1", Nickname: "张三", Content: "第三句"},
	}
	full := BuildChatLog(msgs)
	reset := BuildChatLog(msgs[1:]) // 重置窗口：起点前移，消息集合变了

	for _, want := range []string{`"message_id":502`, `"message_id":503`} {
		if !strings.Contains(full, want) || !strings.Contains(reset, want) {
			t.Fatalf("同一条消息在两个窗口里应是同一个行号 %s：\n全窗口=%s\n重置窗口=%s", want, full, reset)
		}
	}
	if strings.Contains(reset, `"message_id":1,`) {
		t.Fatalf("重置窗口不应重新从 1 编号：%s", reset)
	}
}

// 没有行号的消息（写库失败、测试注入）不带 message_id，也不参与 reply_id 映射。
func TestBuildChatLog无行号的消息不带message_id(t *testing.T) {
	log := BuildChatLog([]Message{
		{RowID: 700, MessageID: "m1", Content: "有行号"},
		{MessageID: "m2", Content: "没行号"},
		{MessageID: "m3", Content: "引用了没行号的那条", ReplyToID: "m2"},
		{MessageID: "m4", Content: ""},
	})
	if got := strings.Count(log, `"message_id"`); got != 1 {
		t.Fatalf("只有有行号的那条该带 message_id，实际 %d 条：%s", got, log)
	}
	if strings.Contains(log, `"reply_id"`) {
		t.Fatalf("被引消息没有行号时应省略 reply_id：%s", log)
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

// TestSetNickname补空名 昵称通常是后学到的：只补同一用户还没名字的消息，
// 已有名字（哪怕是旧名）不动，别的用户也不受影响。
func TestSetNickname补空名(t *testing.T) {
	gc := &GroupMsgCache{
		buf:     make([]Message, 6),
		seenSet: make(map[string]struct{}, 6),
	}
	gc.Add(Message{MessageID: "1", UserID: "u1", Content: "a"})
	gc.Add(Message{MessageID: "2", UserID: "u1", Nickname: "旧名", Content: "b"})
	gc.Add(Message{MessageID: "3", UserID: "u2", Content: "c"})

	gc.SetNickname("u1", "小张")
	gc.SetNickname("", "空ID")
	gc.SetNickname("u2", "")

	all := gc.GetAll()
	if all[0].Nickname != "小张" {
		t.Fatalf("空昵称应被补成小张，实际 %q", all[0].Nickname)
	}
	if all[1].Nickname != "旧名" {
		t.Fatalf("已有昵称不应被覆盖，实际 %q", all[1].Nickname)
	}
	if all[2].Nickname != "" {
		t.Fatalf("空名字不应写入，实际 %q", all[2].Nickname)
	}
}
