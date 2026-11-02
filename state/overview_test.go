package state

import (
	"testing"

	"good-review-master/conversation"
)

// GroupOverviews 要按群返回条数、最近时间、成员数、群名和机器人身份，
// 并且不把没碰过的群串进来。
func TestGroupOverviews按群汇总(t *testing.T) {
	_, scope := openTemp(t)

	scope.SaveMessage(conversation.Message{GroupID: "g1", MessageID: "1", UserID: "u1", Content: "a", Time: 100})
	scope.SaveMessage(conversation.Message{GroupID: "g1", MessageID: "2", UserID: "u2", Content: "b", Time: 300})
	scope.SaveMessage(conversation.Message{GroupID: "g2", MessageID: "3", UserID: "u1", Content: "c", Time: 200})
	scope.SaveGroup("g1", "群一")
	scope.SaveIdentity("g1", "openid-a")
	if _, err := scope.SaveMemberInfo(MemberInfo{GroupID: "g1", UserID: "u1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := scope.SaveMemberInfo(MemberInfo{GroupID: "g1", UserID: "u2"}); err != nil {
		t.Fatal(err)
	}

	list, err := scope.GroupOverviews()
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]GroupOverview, len(list))
	for _, item := range list {
		byID[item.GroupID] = item
	}
	if len(byID) != 2 {
		t.Fatalf("overviews = %+v", byID)
	}
	if g1 := byID["g1"]; g1.MessageCount != 2 || g1.LastTime != 300 || g1.MemberCount != 2 ||
		g1.Name != "群一" || g1.SelfID != "openid-a" {
		t.Fatalf("g1 = %+v", g1)
	}
	if g2 := byID["g2"]; g2.MessageCount != 1 || g2.LastTime != 200 || g2.Name != "" || g2.MemberCount != 0 {
		t.Fatalf("g2 = %+v", g2)
	}
}

// 私聊和群聊一样是 groups 里的一行，用 type 区分。
// 面板群列表只读 groups 这一张小表，私聊就是靠这一行出现的，
// 不用再扫 messages 全表把它捞出来。
func TestGroupOverviews私聊带类型(t *testing.T) {
	_, scope := openTemp(t)

	scope.SaveMessage(conversation.Message{GroupID: "g1", MessageID: "1", Content: "a", Time: 100})
	scope.SaveMessage(conversation.Message{GroupID: "private:u1", MessageID: "2", Content: "b", Time: 200})

	list, err := scope.GroupOverviews()
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]GroupOverview, len(list))
	for _, item := range list {
		byID[item.GroupID] = item
	}
	if g := byID["g1"]; g.Type != "group" || g.MessageCount != 1 {
		t.Fatalf("群聊行 = %+v", g)
	}
	if p := byID["private:u1"]; p.Type != "private" || p.MessageCount != 1 || p.LastTime != 200 {
		t.Fatalf("私聊行 = %+v", p)
	}
}

// 同一个消息重复写入（入站补发、重启回灌）不能把条数翻倍：
// messages 表上 (workspace, group, message_id) 是唯一键。
func TestGroupOverviews重复写入不重复计数(t *testing.T) {
	_, scope := openTemp(t)

	for i := 0; i < 3; i++ {
		scope.SaveMessage(conversation.Message{GroupID: "g1", MessageID: "1", Content: "a", Time: 100})
	}
	list, err := scope.GroupOverviews()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].MessageCount != 1 {
		t.Fatalf("overviews = %+v", list)
	}
}

// 别的工作空间的群不能出现在本工作空间的汇总里。
func TestGroupOverviews按工作空间隔离(t *testing.T) {
	st, scopeA := openTemp(t)
	scopeB := st.Scope("qq_other")

	scopeA.SaveMessage(conversation.Message{GroupID: "g1", MessageID: "1", Content: "a", Time: 100})
	scopeB.SaveMessage(conversation.Message{GroupID: "g2", MessageID: "2", Content: "b", Time: 200})

	list, err := scopeA.GroupOverviews()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].GroupID != "g1" || list[0].MessageCount != 1 {
		t.Fatalf("scopeA = %+v", list)
	}
}

// 清库要连群名的 KV 缓存一起丢掉，否则重置后还能读到上一次的群名。
func TestResetData清空库与群名缓存(t *testing.T) {
	_, scope := openTemp(t)

	scope.SaveMessage(conversation.Message{GroupID: "g1", MessageID: "1", Content: "a", Time: 100})
	scope.SaveGroup("g1", "群一")
	if name := scope.GroupName("g1"); name != "群一" {
		t.Fatalf("群名 = %q", name)
	}

	if err := scope.ResetData(); err != nil {
		t.Fatal(err)
	}
	list, err := scope.GroupOverviews()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("清空后仍有群：%+v", list)
	}
	if name := scope.GroupName("g1"); name != "" {
		t.Fatalf("群名缓存没清：%q", name)
	}
}

// 消息详情页也靠这一列认私聊（标题要显示成「私聊 xxx」而不是 private:xxx），
// 所以它必须跟列表的 GroupOverviews 读到同一个值；查不到的会话返回空串。
func TestGroupType按会话类型返回(t *testing.T) {
	_, scope := openTemp(t)

	scope.SaveMessage(conversation.Message{GroupID: "g1", MessageID: "1", Content: "a", Time: 100})
	scope.SaveMessage(conversation.Message{GroupID: "private:u1", MessageID: "2", Content: "b", Time: 200})

	if got := scope.GroupType("g1"); got != "group" {
		t.Fatalf("群聊 type = %q", got)
	}
	if got := scope.GroupType("private:u1"); got != "private" {
		t.Fatalf("私聊 type = %q", got)
	}
	if got := scope.GroupType("没见过的会话"); got != "" {
		t.Fatalf("不存在的会话 type = %q", got)
	}
}
