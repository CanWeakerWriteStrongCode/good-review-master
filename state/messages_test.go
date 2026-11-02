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
