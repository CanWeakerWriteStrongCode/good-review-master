package server

import (
	"path/filepath"
	"testing"

	"good-review-master/config"
	"good-review-master/conversation"
	"good-review-master/state"
	"good-review-master/workspace"
)

// 面板的数字全部来自 state.db，这里直接喂一份库汇总，验证拼装逻辑。
func TestCollectDetails用库里统计(t *testing.T) {
	stats := map[string]state.GroupOverview{
		"g1": {GroupID: "g1", Name: "群1", MessageCount: 42, MemberCount: 7, LastTime: 1700000000, SelfID: "openid-a"},
		"g2": {GroupID: "g2", Name: "群2", MessageCount: 3, MemberCount: 1, LastTime: 1700000100, SelfID: "openid-b"},
	}
	s := &Server{}
	groups, accounts := s.collectDetails([]string{"g1", "g2", "g3"}, "1906072771", "ws", "小助理", stats)

	if len(groups) != 3 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].GroupName != "群1" || groups[0].MessageCount != 42 || groups[0].MemberCount != 7 ||
		groups[0].SelfID != "openid-a" || !groups[0].HasMessages {
		t.Fatalf("g1 = %+v", groups[0])
	}
	if want := formatTimestamp(1700000000); groups[0].LastActivity != want {
		t.Fatalf("g1 last_activity = %q, want %q", groups[0].LastActivity, want)
	}
	if groups[1].MessageCount != 3 || groups[1].SelfID != "openid-b" {
		t.Fatalf("g2 = %+v", groups[1])
	}
	// 传进来的群号里没有库汇总的（纯内存模式才会出现）：照样列出来，但不带任何统计。
	if groups[2].HasMessages || groups[2].MessageCount != 0 || groups[2].LastActivity != "" {
		t.Fatalf("g3 = %+v", groups[2])
	}

	if len(accounts) != 1 {
		t.Fatalf("accounts = %+v", accounts)
	}
	acc := accounts[0]
	if acc.SelfID != "1906072771" || acc.Nickname != "小助理" ||
		acc.GroupCount != 3 || acc.ActiveGroupCount != 2 || acc.MessageCount != 45 {
		t.Fatalf("账号汇总 = %+v", acc)
	}
	if want := formatTimestamp(1700000100); acc.LastActivity != want {
		t.Fatalf("账号最近活动 = %q, want %q", acc.LastActivity, want)
	}
}

func TestCollectDetails无账号时不造账号行(t *testing.T) {
	s := &Server{}
	groups, accounts := s.collectDetails([]string{"g1"}, "", "", "", nil)
	if len(groups) != 1 {
		t.Fatalf("groups = %+v", groups)
	}
	if groups[0].HasMessages || groups[0].MessageCount != 0 {
		t.Fatalf("没有库汇总时不该有任何数字：%+v", groups[0])
	}
	if len(accounts) != 0 {
		t.Fatalf("没有接收账号时不应生成账号汇总：%+v", accounts)
	}
}

// 没有工作空间（拿不到 state.db）时的降级路径：只按配置列群号，统计全空。
func TestCollectGroupDetails只列配置里的群(t *testing.T) {
	cfg := &config.Config{AllowGroups: []string{"g1", "*"}}
	s := &Server{}
	groups, accounts := s.collectGroupDetails(cfg)
	if len(groups) != 1 || groups[0].GroupID != "g1" || groups[0].HasMessages {
		t.Fatalf("groups = %+v", groups)
	}
	if len(accounts) != 0 {
		t.Fatalf("旧路径没有账号来源，不应生成账号汇总：%+v", accounts)
	}
}

// 群列表以库里有的群为准：配置里列了但库里没记录的群不列。
// bot.allow_groups 是所有账号共用的白名单，里面混着别的账号的群，
// 直接拿来当群列表会多出「未归属账号」。
func TestCollectWorkspaceDetails群列表以库为准(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), state.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	scope := st.Scope("qq_official_bot_1904466648")
	scope.SaveMessage(conversation.Message{
		GroupID: "own", MessageID: "1", UserID: "u1", Content: "a", Time: 100,
	})

	view := workspace.View{
		ID: "qq_official_bot_1904466648", Name: "大肥鱼", Account: "qq_official_bot_1904466648",
		// 配置里还写了别的账号看的群 other，它在本账号库里没有记录。
		Groups: []string{"own", "other"},
		State:  scope, HasState: true,
	}
	s := &Server{}
	groups, accounts := s.collectWorkspaceDetails(view)

	if len(groups) != 1 || groups[0].GroupID != "own" {
		t.Fatalf("只该列出库里有记录的群：%+v", groups)
	}
	if len(accounts) != 1 || accounts[0].GroupCount != 1 || accounts[0].ActiveGroupCount != 1 {
		t.Fatalf("账号汇总 = %+v", accounts)
	}
}

// 状态栏的数字是全局口径：所有账号的群加起来算，不能只算当前选中的那个。
// 之前 /api/workspaces/:wid/groups 返回的是当前工作空间的数字，
// 切一次账号状态栏就从「2 个账号 | 5 个群」变成「1 个账号 | 1 个群」。
func TestBotStatusOf汇总所有账号(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), state.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	big := st.Scope("qq_official_bot_1904466648")
	for _, id := range []string{"g1", "g2", "g3", "g4"} {
		big.SaveMessage(conversation.Message{GroupID: id, MessageID: "m-" + id, UserID: "u1", Content: "x", Time: 100})
	}
	small := st.Scope("qq_official_bot_1906072771")
	small.SaveMessage(conversation.Message{GroupID: "g5", MessageID: "m-g5", UserID: "u1", Content: "y", Time: 200})

	views := []workspace.View{
		{ID: "qq_official_bot_1904466648", Name: "大肥鱼", Account: "qq_official_bot_1904466648", State: big, HasState: true},
		{ID: "qq_official_bot_1906072771", Name: "小助理", Account: "qq_official_bot_1906072771", State: small, HasState: true},
	}

	s := &Server{}
	status := s.botStatusOf(&config.Config{}, views)
	if status.AccountCount != 2 || status.GroupCount != 5 {
		t.Fatalf("状态栏 = %d 个账号 / %d 个群，want 2 / 5", status.AccountCount, status.GroupCount)
	}

	// 只传一个工作空间时就只算那一个的，不能凭空补成全局。
	one := s.botStatusOf(&config.Config{}, views[1:])
	if one.AccountCount != 1 || one.GroupCount != 1 {
		t.Fatalf("单账号 = %d 个账号 / %d 个群，want 1 / 1", one.AccountCount, one.GroupCount)
	}
}

// 群列表按最后一条消息的时间倒序：刚聊过的排最前。
// 这个时间就是汇总查询里的 MAX(ts)，库里的 GroupOverview.LastTime 直接拿来用。
func TestCollectWorkspaceDetails按最近消息倒序(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), state.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	scope := st.Scope("qq_official_bot_1904466648")
	// 写入顺序故意打乱，排出来的结果不能依赖写入先后。
	scope.SaveMessage(conversation.Message{GroupID: "mid", MessageID: "1", UserID: "u1", Content: "a", Time: 500})
	scope.SaveMessage(conversation.Message{GroupID: "new", MessageID: "2", UserID: "u1", Content: "b", Time: 900})
	scope.SaveMessage(conversation.Message{GroupID: "old", MessageID: "3", UserID: "u1", Content: "c", Time: 100})

	view := workspace.View{
		ID: "qq_official_bot_1904466648", Name: "大肥鱼", Account: "qq_official_bot_1904466648",
		State: scope, HasState: true,
	}
	s := &Server{}
	groups, _ := s.collectWorkspaceDetails(view)

	if len(groups) != 3 {
		t.Fatalf("groups = %+v", groups)
	}
	want := []string{"new", "mid", "old"}
	for i, id := range want {
		if groups[i].GroupID != id {
			t.Fatalf("第 %d 个群 = %q，want %q（全部：%+v）", i, groups[i].GroupID, id, groups)
		}
	}
}

// 全部会话列表：把两个账号的群拼成一张表，按最近消息时间倒序。
// 卡片上要写清楚归谁监控，所以每条都带 workspace_id / workspace_name。
func TestCollectSessions跨账号合并倒序(t *testing.T) {
	st, err := state.Open(filepath.Join(t.TempDir(), state.FileName))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	big := st.Scope("qq_official_bot_1904466648")
	big.SaveMessage(conversation.Message{GroupID: "g-old", MessageID: "1", UserID: "u1", Content: "a", Time: 100})
	big.SaveMessage(conversation.Message{GroupID: "g-new", MessageID: "2", UserID: "u1", Content: "b", Time: 900})
	small := st.Scope("qq_official_bot_1906072771")
	small.SaveMessage(conversation.Message{GroupID: "g-mid", MessageID: "3", UserID: "u1", Content: "c", Time: 500})

	views := []workspace.View{
		{ID: "qq_official_bot_1904466648", Name: "大肥鱼", Account: "qq_official_bot_1904466648", State: big, HasState: true},
		{ID: "qq_official_bot_1906072771", Name: "小助理", Account: "qq_official_bot_1906072771", State: small, HasState: true},
	}

	s := &Server{}
	list := s.collectSessions(views)
	if len(list) != 3 {
		t.Fatalf("会话数 = %d，want 3（%+v）", len(list), list)
	}

	// 两个账号的群交替出现，说明排的是全局时间而不是按账号一块一块。
	want := []struct{ ws, group string }{
		{"qq_official_bot_1904466648", "g-new"},
		{"qq_official_bot_1906072771", "g-mid"},
		{"qq_official_bot_1904466648", "g-old"},
	}
	for i, w := range want {
		if list[i].WorkspaceID != w.ws || list[i].GroupID != w.group {
			t.Fatalf("第 %d 条 = %s/%s，want %s/%s（全部：%+v）",
				i, list[i].WorkspaceID, list[i].GroupID, w.ws, w.group, list)
		}
	}
	if list[0].WorkspaceName != "大肥鱼" || list[1].WorkspaceName != "小助理" {
		t.Fatalf("归属账号名没带上：%+v", list)
	}
}
