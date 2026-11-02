package state

import (
	"strconv"
	"testing"

	"good-review-master/conversation"
)

// 详情页要的是最近 N 条，倒序展示（最新的在最上面）；总数要单独给，不能拿返回条数当总数。
func TestMessages按时间倒序并封顶(t *testing.T) {
	_, scope := openTemp(t)
	for i := 0; i < 5; i++ {
		scope.SaveMessage(conversation.Message{
			GroupID: "g1", MessageID: strconv.Itoa(i), UserID: "u1",
			Content: "m" + strconv.Itoa(i), Time: int64(100 + i),
		})
	}

	msgs, total, err := scope.Messages("g1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if total != 5 {
		t.Fatalf("库里总条数 = %d", total)
	}
	if len(msgs) != 3 {
		t.Fatalf("返回条数 = %d", len(msgs))
	}
	if msgs[0].Content != "m4" || msgs[2].Content != "m2" {
		t.Fatalf("取的不是最近三条：%+v", msgs)
	}
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Time > msgs[i-1].Time {
			t.Fatalf("时间没按倒序：%+v", msgs)
		}
	}
}

// messages 表只存 user_id，昵称在 members 表里，读消息时要补上。
func TestMessages补成员昵称(t *testing.T) {
	_, scope := openTemp(t)
	scope.SaveMessage(conversation.Message{GroupID: "g1", MessageID: "1", UserID: "u1", Content: "hi", Time: 100})
	if _, err := scope.SaveMemberInfo(MemberInfo{
		GroupID: "g1", UserID: "u1", Name: "张三", DisplayName: "老张",
	}); err != nil {
		t.Fatal(err)
	}

	msgs, _, err := scope.Messages("g1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Nickname != "张三" || msgs[0].Card != "老张" {
		t.Fatalf("昵称没补上：%+v", msgs)
	}
}

// 别的群的消息不能串进来；没消息的群返回空表 + 0，而不是 nil。
func TestMessages按群隔离(t *testing.T) {
	_, scope := openTemp(t)
	scope.SaveMessage(conversation.Message{GroupID: "g1", MessageID: "1", Content: "a", Time: 100})
	scope.SaveMessage(conversation.Message{GroupID: "g2", MessageID: "2", Content: "b", Time: 200})

	msgs, total, err := scope.Messages("g1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(msgs) != 1 || msgs[0].GroupID != "g1" {
		t.Fatalf("g1 = %+v total=%d", msgs, total)
	}

	empty, total, err := scope.Messages("g9", 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || empty == nil || len(empty) != 0 {
		t.Fatalf("空群 = %+v total=%d", empty, total)
	}
}

// 详情页也要带图片描述：正文里只有 [图片] 占位，描述在全局图库。
// 多张图按 ImageIDs 的顺序各回各的；图库里没有的那张留空，不能串位。
func TestMessages回填图片描述(t *testing.T) {
	st, scope := openTemp(t)
	described := conversation.ImageID{SHA: "s-cat", Size: 10}
	unknown := conversation.ImageID{SHA: "s-unknown", Size: 20}
	if err := st.SaveImageNote(conversation.ImageNote{
		ImageID: described, Note: "橘猫趴在键盘上", InfoLevel: "high", Status: "ok",
	}); err != nil {
		t.Fatal(err)
	}
	scope.SaveMessage(conversation.Message{
		GroupID: "g1", MessageID: "m1", UserID: "u1", Content: "看这个 [图片][图片]", Time: 100,
		ImageIDs: []conversation.ImageID{described, unknown},
	})

	msgs, _, err := scope.Messages("g1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || len(msgs[0].ImageNotes) != 2 {
		t.Fatalf("两张图应各回填一条描述占位：%+v", msgs)
	}
	if got := msgs[0].ImageNotes[0].Note; got != "橘猫趴在键盘上" {
		t.Fatalf("第一张图的描述 = %q", got)
	}
	if got := msgs[0].ImageNotes[1].Note; got != "" {
		t.Fatalf("图库里没有的图应留空，实际 %q", got)
	}
}

// 机器人自己的回复在拿不到身份时只能存空 user_id；身份学到后回填。
// openid 是账号级的，所以一次要把所有群都补上；别人说过的话一条都不能动。
func TestBackfillSelfUserID只填空发送者(t *testing.T) {
	_, scope := openTemp(t)
	scope.SaveMessage(conversation.Message{GroupID: "g1", MessageID: "m1", UserID: "u1", Content: "群友说的", Time: 100})
	scope.SaveMessage(conversation.Message{GroupID: "g1", MessageID: "bot-1", Content: "机器人说的", Time: 200})
	scope.SaveMessage(conversation.Message{GroupID: "g2", MessageID: "bot-2", Content: "别的群也是它说的", Time: 300})
	// 入站时没带 user_id 的怪消息：没有 bot- 前缀，不能当成机器人自己的话。
	scope.SaveMessage(conversation.Message{GroupID: "g1", MessageID: "m2", Content: "来源不明", Time: 400})

	rows, err := scope.BackfillSelfUserID("openid-self")
	if err != nil {
		t.Fatal(err)
	}
	if rows != 2 {
		t.Fatalf("两个群的机器人消息都应补上，实际 %d 行", rows)
	}

	msgs, _, err := scope.Messages("g1", 10)
	if err != nil {
		t.Fatal(err)
	}
	// 按时间倒序：m2、bot-1、m1
	if len(msgs) != 3 || msgs[1].UserID != "openid-self" {
		t.Fatalf("g1 的机器人消息应补上 ID：%+v", msgs)
	}
	if msgs[2].UserID != "u1" {
		t.Fatalf("别人的发送者被改了：%+v", msgs[2])
	}
	if msgs[0].UserID != "" {
		t.Fatalf("不是机器人发的空发送者不该被填：%+v", msgs[0])
	}

	// 别的群不用再学一次，同一个 openid 已经把它补掉了。
	other, _, err := scope.Messages("g2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].UserID != "openid-self" {
		t.Fatalf("别的群应一起补上：%+v", other)
	}

	// 再跑一次是空转：已经没有待补的行了。
	if again, err := scope.BackfillSelfUserID("openid-self"); err != nil || again != 0 {
		t.Fatalf("重复回填应改 0 行，实际 %d, %v", again, err)
	}
}
