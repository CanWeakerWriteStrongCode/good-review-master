package state

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"good-review-master/conversation"
	"good-review-master/logutil"
)

// TestMain 隔离日志目录并初始化 logger：迁移路径会打 WARN，
// 而 logutil 未初始化时是 nil 指针。
func TestMain(m *testing.M) {
	original, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	temp, err := os.MkdirTemp("", "state-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(temp); err != nil {
		panic(err)
	}
	logutil.SetupLogger()

	code := m.Run()

	logutil.Close()
	_ = os.Chdir(original)
	_ = os.RemoveAll(temp)
	os.Exit(code)
}

func openTemp(t *testing.T) (*Store, *Scope) {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), FileName))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, st.Scope("qq_test")
}

// waitQueue 保留旧调用点：消息现在是同步写入，无需等待。
func waitQueue(t *testing.T, st *Store) {
	t.Helper()
	_, _ = st.Stats()
}

func execDB(t *testing.T, st *Store, statement string) {
	t.Helper()
	if err := st.db.Exec(statement).Error; err != nil {
		t.Fatalf("执行 %q 失败：%v", statement, err)
	}
}

func Test消息持久化与按群截断(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := st.Scope("qq_a")
	for i := 1; i <= 5; i++ {
		scope.SaveMessage(conversation.Message{
			MessageID: string(rune('0' + i)), GroupID: "g1", UserID: "u",
			Content: "消息", Images: []string{"https://example.com/a.png"},
			ReplyToID: "1", Time: int64(i),
		})
	}
	scope.SaveMessage(conversation.Message{MessageID: "x", GroupID: "g2", UserID: "u", Content: "另一个群", Time: 9})
	waitQueue(t, st)

	// 内存窗口只留最近 N 条，但库里一条都不能少
	msgs, err := scope.LoadRecent(2)
	if err != nil {
		t.Fatalf("LoadRecent: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("每个群最多 2 条，应得到 3 条（g1:2 + g2:1），实际 %d", len(msgs))
	}
	rows, _ := st.Stats()
	if rows != 6 {
		t.Fatalf("库里应保留全部 6 条，实际 %d", rows)
	}
	_ = st.Close()

	st, err = Open(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer st.Close()
	scope = st.Scope("qq_a")
	rows, _ = st.Stats()
	if rows != 6 {
		t.Fatalf("重启后应仍是 6 条，实际 %d", rows)
	}
	msgs, err = scope.LoadRecent(10)
	if err != nil {
		t.Fatalf("LoadRecent: %v", err)
	}
	if len(msgs) != 6 || msgs[0].GroupID != "g1" || msgs[0].MessageID != "1" {
		t.Fatalf("消息顺序或内容丢失：%+v", msgs)
	}
	if len(msgs[0].Images) != 1 || msgs[0].ReplyToID != "1" {
		t.Fatalf("图片/引用未恢复：%+v", msgs[0])
	}
}

// Test工作空间隔离 锁住"同表按 workspace_id 区分"：
// 同一个群、同一条 message_id 在两个账号下必须互不覆盖。
func Test工作空间隔离(t *testing.T) {
	st, _ := openTemp(t)
	a := st.Scope("qq_a")
	b := st.Scope("qq_b")
	a.SaveMessage(conversation.Message{MessageID: "1", GroupID: "g1", Content: "from-a", Time: 1})
	b.SaveMessage(conversation.Message{MessageID: "1", GroupID: "g1", Content: "from-b", Time: 2})
	waitQueue(t, st)

	msgsA, err := a.LoadRecent(10)
	if err != nil || len(msgsA) != 1 || msgsA[0].Content != "from-a" {
		t.Fatalf("A 读取到 %+v, %v", msgsA, err)
	}
	msgsB, err := b.LoadRecent(10)
	if err != nil || len(msgsB) != 1 || msgsB[0].Content != "from-b" {
		t.Fatalf("B 读取到 %+v, %v", msgsB, err)
	}

	a.SaveAnchor("g1", conversation.LLMAnchor{Start: "a1"})
	b.SaveAnchor("g1", conversation.LLMAnchor{Start: "b1"})
	anchorsA, _ := a.LoadAnchors()
	anchorsB, _ := b.LoadAnchors()
	if anchorsA["g1"].Start != "a1" || anchorsB["g1"].Start != "b1" {
		t.Fatalf("锚点串台：A=%+v B=%+v", anchorsA, anchorsB)
	}

	a.SaveGroupPersona("g1", "人格A")
	b.SaveGroupPersona("g1", "人格B")
	pa, _ := a.LoadGroupPersonas()
	pb, _ := b.LoadGroupPersonas()
	if pa["g1"] != "人格A" || pb["g1"] != "人格B" {
		t.Fatalf("群人格串台：A=%+v B=%+v", pa, pb)
	}
}

func Test锚点成员人格与元信息(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := st.Scope("qq_a")

	scope.SaveAnchor("g1", conversation.LLMAnchor{Start: "10", LastSent: "20"})
	scope.SaveSelfUserID("openid-a")
	scope.SaveMember("g1", "u1", "张三", "老张")
	scope.SaveGroupPersona("g1", "锐评下")
	if err := scope.SaveProfile(WorkspaceProfile{Name: "小助理"}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st, err = Open(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer st.Close()
	scope = st.Scope("qq_a")

	anchors, err := scope.LoadAnchors()
	if err != nil || anchors["g1"].Start != "10" || anchors["g1"].LastSent != "20" {
		t.Fatalf("anchors = %+v, %v", anchors, err)
	}
	// SaveAnchor 没给 At 时要回填写入时刻：锚点重启后能不能用，全靠这个时间判过期。
	if age := time.Since(anchors["g1"].At); age < 0 || age > time.Minute {
		t.Fatalf("At 应为写入时刻，得到 %v（距今 %v）", anchors["g1"].At, age)
	}
	if selfUserID, err := scope.LoadSelfUserID(); err != nil || selfUserID != "openid-a" {
		t.Fatalf("self_user_id = %q, %v", selfUserID, err)
	}
	members, err := scope.LoadMembers()
	if err != nil || members["g1"]["u1"] != "老张" {
		t.Fatalf("members = %+v, %v", members, err)
	}
	personas, err := scope.LoadGroupPersonas()
	if err != nil || personas["g1"] != "锐评下" {
		t.Fatalf("personas = %+v, %v", personas, err)
	}
	if profile, ok, err := scope.LoadProfile(); err != nil || !ok || profile.Name != "小助理" {
		t.Fatalf("profile = %+v, %v, %v", profile, ok, err)
	}
	if _, ok, err := st.Scope("qq_other").LoadProfile(); err != nil || ok {
		t.Fatalf("profile 不应跨工作空间泄漏，ok=%v err=%v", ok, err)
	}

	scope.DeleteGroupPersona("g1")
	personas, _ = scope.LoadGroupPersonas()
	if _, ok := personas["g1"]; ok {
		t.Fatal("删除人格后不应再读到")
	}
}

func Test关闭后不panic(t *testing.T) {
	st, scope := openTemp(t)
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// 关闭后再写：必须静默丢弃，不能往已关闭的 channel 发送而 panic
	scope.SaveMessage(conversation.Message{MessageID: "1", GroupID: "g"})
	scope.SaveAnchor("g", conversation.LLMAnchor{Start: "1"})
	if filepath.Base(st.path) != FileName {
		t.Fatalf("path = %s", st.path)
	}
}

func TestOpen路径是目录时返回错误(t *testing.T) {
	// 这种情况下 sqlite 无法打开；调用方据此降级为纯内存。
	dir := t.TempDir()
	if _, err := Open(dir); err == nil {
		t.Fatal("路径是目录时应返回错误")
	}
}

// Test旧结构迁移 保证从"每工作空间一个库"升级时不会因为缺 workspace_id 而查询失败。
func Test旧结构迁移(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// 手工退回 v1 的表结构（无 workspace_id），模拟旧库。
	execDB(t, st, `DROP TABLE messages`)
	execDB(t, st, `CREATE TABLE messages (group_id TEXT, message_id TEXT, user_id TEXT, content TEXT, images TEXT, reply_to TEXT, ts INTEGER)`)
	execDB(t, st, `PRAGMA user_version = 1`)
	_ = st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatalf("迁移后应能打开：%v", err)
	}
	defer st.Close()
	scope := st.Scope("qq_a")
	scope.SaveMessage(conversation.Message{MessageID: "1", GroupID: "g", Content: "ok", Time: 1})
	waitQueue(t, st)
	msgs, err := scope.LoadRecent(1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("迁移后的表应可读写：%+v, %v", msgs, err)
	}
}

// Test群档案读写 群名可以为空；空名不能覆盖已记录的名字。
func Test群档案读写(t *testing.T) {
	_, scope := openTemp(t)
	scope.SaveGroup("g1", "")
	scope.SaveGroup("g1", "测试群")
	scope.SaveGroup("g1", "")

	groups, err := scope.LoadGroups()
	if err != nil {
		t.Fatalf("LoadGroups: %v", err)
	}
	if groups["g1"] != "测试群" {
		t.Fatalf("群名应保留非空值，得到 %q", groups["g1"])
	}
}

// Testv2升级保留消息 v2→v3 只补 groups 表，不能重建已有表把消息清掉。
func TestV2升级保留消息(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := st.Scope("qq_a")
	scope.SaveMessage(conversation.Message{MessageID: "m1", GroupID: "g1", Content: "旧消息", Time: 1})
	waitQueue(t, st)

	// 手动退回 v2：没有 groups 表。
	execDB(t, st, `DROP TABLE groups`)
	execDB(t, st, `PRAGMA user_version = 2`)
	_ = st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatalf("升级后应能打开：%v", err)
	}
	defer st.Close()
	scope = st.Scope("qq_a")
	msgs, err := scope.LoadRecent(10)
	if err != nil || len(msgs) != 1 || msgs[0].Content != "旧消息" {
		t.Fatalf("升级不能丢消息：%+v, %v", msgs, err)
	}
	scope.SaveGroup("g1", "升级后的群")
	groups, err := scope.LoadGroups()
	if err != nil || groups["g1"] != "升级后的群" {
		t.Fatalf("升级后 groups 应可读写：%+v, %v", groups, err)
	}
}

func TestWorkspaces返回平台类型与账号(t *testing.T) {
	st, scope := openTemp(t)
	if err := scope.SaveProfile(WorkspaceProfile{
		Platform: "qq", AccountKind: "official_bot", Account: "1906",
	}); err != nil {
		t.Fatal(err)
	}
	other := st.Scope("qq_b")
	if err := other.SaveProfile(WorkspaceProfile{
		Platform: "qq", AccountKind: "personal_account", Account: "b",
	}); err != nil {
		t.Fatal(err)
	}

	metas, err := st.Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(metas) != 2 {
		t.Fatalf("应有 2 个工作空间，实际 %+v", metas)
	}
	byID := map[string]WorkspaceMeta{}
	for _, meta := range metas {
		byID[meta.ID] = meta
	}
	if got := byID["qq_test"]; got.Platform != "qq" ||
		got.AccountKind != "official_bot" || got.Account != "1906" {
		t.Fatalf("qq_test meta = %+v", got)
	}
}

// Test引用索引反查 被引消息存下自己的 ref_idx，新消息用 ref_msg_idx 反查 message_id。
func Test引用索引反查(t *testing.T) {
	st, scope := openTemp(t)
	scope.SaveMessage(conversation.Message{
		MessageID: "ROBOT1.0_ABC", GroupID: "g1", UserID: "u1",
		Content: "✅ 已切换人格", RefIdx: "REFIDX_Q", Time: 1,
	})
	waitQueue(t, st)

	if got := scope.ResolveMessageID("g1", "REFIDX_Q"); got != "ROBOT1.0_ABC" {
		t.Fatalf("反查 = %q, want ROBOT1.0_ABC", got)
	}
	if got := scope.ResolveMessageID("g1", "REFIDX_UNKNOWN"); got != "" {
		t.Fatalf("未知索引应为空，got %q", got)
	}
	if got := scope.ResolveMessageID("other", "REFIDX_Q"); got != "" {
		t.Fatalf("别的群不能命中，got %q", got)
	}
}

// TestV3升级补引用索引 v3 老库没有 ref_idx 列，升级只补列，消息不能丢。
func TestV3升级补引用索引(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE messages (
			workspace_id TEXT NOT NULL, group_id TEXT NOT NULL, message_id TEXT NOT NULL,
			user_id TEXT NOT NULL, content TEXT NOT NULL, images TEXT NOT NULL DEFAULT '[]',
			reply_to TEXT NOT NULL DEFAULT '', ts INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id, message_id))`,
		`INSERT INTO messages (workspace_id, group_id, message_id, user_id, content, ts)
			VALUES ('qq_a', 'g1', 'm1', 'u1', '旧消息', 1)`,
		`PRAGMA user_version = 3`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("造 v3 库失败：%v", err)
		}
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("升级后应能打开：%v", err)
	}
	defer st.Close()
	scope := st.Scope("qq_a")
	msgs, err := scope.LoadRecent(10)
	if err != nil || len(msgs) != 1 || msgs[0].Content != "旧消息" {
		t.Fatalf("升级不能丢消息：%+v, %v", msgs, err)
	}
	scope.SaveMessage(conversation.Message{MessageID: "m2", GroupID: "g1", Content: "新消息", RefIdx: "REFIDX_N", Time: 2})
	waitQueue(t, st)
	if got := scope.ResolveMessageID("g1", "REFIDX_N"); got != "m2" {
		t.Fatalf("补列后应可写 ref_idx：%q", got)
	}
}

// TestSaveMessage返回自增行号 行号就是发给大模型的 message_id：
// 写一次就要拿到，重写同一条（upsert）不换号、也不新增行。
func TestSaveMessage返回自增行号(t *testing.T) {
	_, scope := openTemp(t)

	first := scope.SaveMessage(conversation.Message{MessageID: "m1", GroupID: "g1", Content: "一", Time: 1})
	second := scope.SaveMessage(conversation.Message{MessageID: "m2", GroupID: "g1", Content: "二", Time: 2})
	if first <= 0 || second <= 0 || second <= first {
		t.Fatalf("行号应依次自增，实际 %d / %d", first, second)
	}

	// 同一条重写（实现端回传机器人自己的消息）：行号不变，内容更新
	if again := scope.SaveMessage(conversation.Message{MessageID: "m1", GroupID: "g1", Content: "一改", Time: 1}); again != first {
		t.Fatalf("重写同一条应返回原行号 %d，实际 %d", first, again)
	}
	msgs, err := scope.LoadRecent(10)
	if err != nil {
		t.Fatalf("LoadRecent: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("重写不应新增行，实际 %d 条：%+v", len(msgs), msgs)
	}
	if msgs[0].RowID != first || msgs[0].Content != "一改" || msgs[1].RowID != second {
		t.Fatalf("行号或内容不对：%+v", msgs)
	}
}

// TestV10升级回填群统计 v10 的 groups 只有群名，面板每次打开都要扫 messages 全表算条数。
// v11 把条数/成员数/最后时间存进 groups，升级时得拿存量数据回填一遍；
// 只发过消息、从没建过档案的会话（包括私聊）也要补出行来，否则升级后面板上就少了。
func TestV10升级回填群统计(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			workspace_id TEXT NOT NULL, group_id TEXT NOT NULL, message_id TEXT NOT NULL,
			user_id TEXT NOT NULL, content TEXT NOT NULL, images TEXT NOT NULL DEFAULT '[]',
			image_ids TEXT NOT NULL DEFAULT '[]', reply_to TEXT NOT NULL DEFAULT '',
			ref_idx TEXT NOT NULL DEFAULT '', ts INTEGER NOT NULL,
			UNIQUE (workspace_id, group_id, message_id))`,
		`CREATE TABLE members (
			workspace_id TEXT NOT NULL, group_id TEXT NOT NULL, user_id TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL DEFAULT '',
			extra TEXT NOT NULL DEFAULT '{}', updated_at INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id, user_id))`,
		`CREATE TABLE groups (
			workspace_id TEXT NOT NULL, group_id TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
			extra TEXT NOT NULL DEFAULT '{}', updated_at INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id))`,
		`INSERT INTO messages (workspace_id, group_id, message_id, user_id, content, ts) VALUES
			('qq_a', 'g1', 'm1', 'u1', '一', 100),
			('qq_a', 'g1', 'm2', 'u2', '二', 300),
			('qq_a', 'private:u9', 'm3', 'u9', '私聊', 200)`,
		`INSERT INTO members (workspace_id, group_id, user_id, updated_at) VALUES
			('qq_a', 'g1', 'u1', 1), ('qq_a', 'g1', 'u2', 1)`,
		`INSERT INTO groups (workspace_id, group_id, name, updated_at)
			VALUES ('qq_a', 'g1', '群一', 1)`,
		`PRAGMA user_version = 10`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("造 v10 库失败：%v", err)
		}
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("升级后应能打开：%v", err)
	}
	defer st.Close()

	list, err := st.Scope("qq_a").GroupOverviews()
	if err != nil {
		t.Fatalf("GroupOverviews: %v", err)
	}
	byID := make(map[string]GroupOverview, len(list))
	for _, item := range list {
		byID[item.GroupID] = item
	}
	if len(byID) != 2 {
		t.Fatalf("升级后应有群 + 私聊两行：%+v", byID)
	}
	if g := byID["g1"]; g.Type != "group" || g.MessageCount != 2 || g.MemberCount != 2 ||
		g.LastTime != 300 || g.Name != "群一" {
		t.Fatalf("g1 回填不对：%+v", g)
	}
	if p := byID["private:u9"]; p.Type != "private" || p.MessageCount != 1 ||
		p.MemberCount != 0 || p.LastTime != 200 {
		t.Fatalf("私聊回填不对：%+v", p)
	}

	// 升级后触发器要开始工作：再来一条新消息，条数接着加。
	st.Scope("qq_a").SaveMessage(conversation.Message{MessageID: "m4", GroupID: "g1", Content: "新", Time: 400})
	waitQueue(t, st)
	list, err = st.Scope("qq_a").GroupOverviews()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range list {
		if item.GroupID == "g1" && (item.MessageCount != 3 || item.LastTime != 400) {
			t.Fatalf("升级后计数不跟着新消息走：%+v", item)
		}
	}
}

// TestV11升级把身份并进群档案 v11 之前 identities 是单独一张表，主键和 groups
// 一样都是 (workspace_id, group_id)。v12 并成 groups.self_user_id，v15 又把身份
// 提到 workspace_profile.self_user_id（openid 是账号级的，各群同一个值）。
// 升级时这两步得连起来：身份要一路搬到账号级，只有身份没有消息的会话也别丢档案行。
func TestV11升级把身份并进群档案(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE groups (
			workspace_id TEXT NOT NULL, group_id TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL DEFAULT 'group', message_count INTEGER NOT NULL DEFAULT 0,
			member_count INTEGER NOT NULL DEFAULT 0, last_ts INTEGER NOT NULL DEFAULT 0,
			extra TEXT NOT NULL DEFAULT '{}', updated_at INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id))`,
		`CREATE TABLE identities (
			workspace_id TEXT NOT NULL, group_id TEXT NOT NULL, self_user_id TEXT NOT NULL,
			learned_at INTEGER NOT NULL, PRIMARY KEY (workspace_id, group_id))`,
		`INSERT INTO groups
			(workspace_id, group_id, name, type, message_count, member_count, last_ts, updated_at)
			VALUES ('qq_a', 'g1', '群一', 'group', 2, 1, 300, 500)`,
		// g1 有档案也有身份；g9 只有身份没有消息，升级后得补出一行。
		`INSERT INTO identities (workspace_id, group_id, self_user_id, learned_at) VALUES
			('qq_a', 'g1', 'openid-a', 100),
			('qq_a', 'g9', 'openid-x', 200)`,
		`PRAGMA user_version = 11`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("造 v11 库失败：%v", err)
		}
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("升级后应能打开：%v", err)
	}
	defer st.Close()

	var left int
	st.db.Raw(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'identities'`).Scan(&left)
	if left != 0 {
		t.Fatal("identities 表应已删掉")
	}

	list, err := st.Scope("qq_a").GroupOverviews()
	if err != nil {
		t.Fatalf("GroupOverviews: %v", err)
	}
	byID := make(map[string]GroupOverview, len(list))
	for _, item := range list {
		byID[item.GroupID] = item
	}
	if len(byID) != 2 {
		t.Fatalf("升级后应有 g1 和 g9 两行：%+v", byID)
	}
	// 原有的计数和群名不能被身份搬移冲掉。身份现在是账号级的：两个群读到的都是
	// promoteSelfUserID 挑出来的那一条（按 group_id 序取第一个真值，即 g1 的 openid-a）。
	if g := byID["g1"]; g.SelfID != "openid-a" || g.Name != "群一" ||
		g.MessageCount != 2 || g.MemberCount != 1 || g.LastTime != 300 {
		t.Fatalf("g1 迁移不对：%+v", g)
	}
	if g := byID["g9"]; g.SelfID != "openid-a" || g.Type != "group" || g.MessageCount != 0 {
		t.Fatalf("g9 补行不对：%+v", g)
	}

	if selfUserID, err := st.Scope("qq_a").LoadSelfUserID(); err != nil || selfUserID != "openid-a" {
		t.Fatalf("self_user_id = %q, %v", selfUserID, err)
	}

	// 旧列搬完就该删掉：留着就是同一个事实的两份副本，迟早会不一致。
	var oldColumn int
	st.db.Raw(`SELECT COUNT(*) FROM pragma_table_info('groups') WHERE name = 'self_user_id'`).Scan(&oldColumn)
	if oldColumn != 0 {
		t.Fatal("groups.self_user_id 应已删掉")
	}

	// 档案更新时间不该被身份覆盖（g1 原来就是 500）。
	var updatedAt int64
	st.db.Raw(`SELECT updated_at FROM groups WHERE workspace_id = 'qq_a' AND group_id = 'g1'`).Scan(&updatedAt)
	if updatedAt != 500 {
		t.Fatalf("updated_at 被覆盖成 %d", updatedAt)
	}
}

// TestV12升级去掉群人格表 v13 把 group_personas 整张表去掉，人格改存 groups.persona_keyword。
// 旧表里的关键字不搬：升级后那几行直接没了，但群档案本身（群名/计数）必须原样留着，
// 补出来的列得能正常读写；同一轮升级里 v15 还会把 groups.self_user_id 提到账号级。
func TestV12升级去掉群人格表(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE groups (
			workspace_id TEXT NOT NULL, group_id TEXT NOT NULL, name TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL DEFAULT 'group', self_user_id TEXT NOT NULL DEFAULT '',
			message_count INTEGER NOT NULL DEFAULT 0, member_count INTEGER NOT NULL DEFAULT 0,
			last_ts INTEGER NOT NULL DEFAULT 0, extra TEXT NOT NULL DEFAULT '{}',
			updated_at INTEGER NOT NULL, PRIMARY KEY (workspace_id, group_id))`,
		`CREATE TABLE group_personas (
			workspace_id TEXT NOT NULL, group_id TEXT NOT NULL, keyword TEXT NOT NULL,
			PRIMARY KEY (workspace_id, group_id))`,
		`INSERT INTO groups
			(workspace_id, group_id, name, type, self_user_id, message_count, member_count, last_ts, updated_at)
			VALUES ('qq_a', 'g1', '群一', 'group', 'openid-a', 2, 1, 300, 500)`,
		`INSERT INTO group_personas (workspace_id, group_id, keyword) VALUES
			('qq_a', 'g1', '锐评下'),
			('qq_a', 'g9', '温柔')`,
		`PRAGMA user_version = 12`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("造 v12 库失败：%v", err)
		}
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("升级后应能打开：%v", err)
	}
	defer st.Close()

	var left int
	st.db.Raw(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'group_personas'`).Scan(&left)
	if left != 0 {
		t.Fatal("group_personas 表应已删掉")
	}

	// 旧人格不搬，也不该凭空补出群档案行：sqlite_master 里不见了、groups 还是原来那一行。
	var rows int
	st.db.Raw(`SELECT COUNT(*) FROM groups WHERE workspace_id = 'qq_a'`).Scan(&rows)
	if rows != 1 {
		t.Fatalf("groups 行数应保持 1，实际 %d", rows)
	}

	scope := st.Scope("qq_a")
	if personas, err := scope.LoadGroupPersonas(); err != nil || len(personas) != 0 {
		t.Fatalf("升级后不应有人格残留：%+v, %v", personas, err)
	}

	// 原有档案不能被补列冲掉；身份同时被提到账号级（v15），groups 上的旧列删掉。
	var name string
	var messages, last int64
	if err := st.db.Raw(`SELECT name, message_count, last_ts FROM groups
		WHERE workspace_id = 'qq_a' AND group_id = 'g1'`).Row().Scan(&name, &messages, &last); err != nil {
		t.Fatalf("读 g1 档案失败：%v", err)
	}
	if name != "群一" || messages != 2 || last != 300 {
		t.Fatalf("g1 升级后档案不对：name=%q messages=%d last=%d", name, messages, last)
	}
	if selfUserID, err := scope.LoadSelfUserID(); err != nil || selfUserID != "openid-a" {
		t.Fatalf("self_user_id = %q, %v", selfUserID, err)
	}

	// 补出来的列能正常读写：#切换人格 → 读回来 → #取消人格，档案行留着。
	scope.SaveGroupPersona("g1", "锐评下")
	if personas, err := scope.LoadGroupPersonas(); err != nil || personas["g1"] != "锐评下" {
		t.Fatalf("写入后 personas = %+v, %v", personas, err)
	}
	scope.DeleteGroupPersona("g1")
	if personas, _ := scope.LoadGroupPersonas(); len(personas) != 0 {
		t.Fatalf("取消人格后 personas = %+v", personas)
	}
	var remains int
	st.db.Raw(`SELECT COUNT(*) FROM groups WHERE workspace_id = 'qq_a' AND group_id = 'g1'`).Scan(&remains)
	if remains != 1 {
		t.Fatal("取消人格不该删掉群档案行")
	}
}

// Test身份与群档案各写各的 身份 v15 起存在 workspace_profile、群档案还在 groups，
// 两张表谁先写都不能影响另一方：写身份不能刷新群档案的 updated_at，写群名也不能
// 把身份冲掉——而且身份是账号级的，每个群读到的都是同一个值。
func Test身份与群档案各写各的(t *testing.T) {
	_, scope := openTemp(t)

	// 顺序一：身份先到，群名后到。
	scope.SaveSelfUserID("openid-a")
	changed, err := scope.SaveGroupInfo("g1", "群一", "group_info", "", "{}")
	if err != nil || !changed {
		t.Fatalf("首次写群名应记为变更：changed=%v err=%v", changed, err)
	}
	var historyCount int
	scope.store.db.Raw(`SELECT COUNT(*) FROM group_info_history
		WHERE workspace_id = ? AND group_id = 'g1'`, scope.workspaceID).Scan(&historyCount)
	if historyCount != 1 {
		t.Fatalf("群名首次写入应记一条历史，实际 %d 条", historyCount)
	}

	// 再来新身份：群名不能被清掉，档案更新时间也不该被刷新。
	// 先把 updated_at 压成一个人造值，免得和“同一秒内写入”混淆。
	scope.store.db.Exec(`UPDATE groups SET updated_at = 12345 WHERE workspace_id = ? AND group_id = 'g1'`,
		scope.workspaceID)
	scope.SaveSelfUserID("openid-b")
	if name := scope.GroupName("g1"); name != "群一" {
		t.Fatalf("写身份把群名冲掉了：%q", name)
	}
	var updatedAt int64
	scope.store.db.Raw(`SELECT updated_at FROM groups WHERE workspace_id = ? AND group_id = 'g1'`,
		scope.workspaceID).Scan(&updatedAt)
	if updatedAt != 12345 {
		t.Fatalf("写身份把档案更新时间冲成了 %d", updatedAt)
	}

	// 顺序二：群名先到，身份后到。
	if _, err := scope.SaveGroupInfo("g2", "群二", "group_info", "", "{}"); err != nil {
		t.Fatal(err)
	}
	scope.SaveSelfUserID("openid-c")
	if name := scope.GroupName("g2"); name != "群二" {
		t.Fatalf("群名 = %q", name)
	}

	list, err := scope.GroupOverviews()
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]GroupOverview, len(list))
	for _, item := range list {
		byID[item.GroupID] = item
	}
	// 同一个账号下所有会话共用一条身份：g1 学到的 openid-b 会被后来的 openid-c 覆盖，
	// 两个群一起变。
	if byID["g1"].SelfID != "openid-c" || byID["g1"].Name != "群一" {
		t.Fatalf("g1 = %+v", byID["g1"])
	}
	if byID["g2"].SelfID != "openid-c" || byID["g2"].Name != "群二" {
		t.Fatalf("g2 = %+v", byID["g2"])
	}
}

// TestV9升级anchors列改名 老库的 anchors 用的是 start_id / last_sent_id，
// 光看名字分不清说的是窗口的起止还是时间点；v10 改成 window_first_id / window_last_id。
// 改名不能丢数据：锚点丢了等于把这个群"上次发到哪"清零，重启后要从头攒前缀。
func TestV9升级anchors列改名(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE anchors (
			workspace_id TEXT NOT NULL, group_id TEXT NOT NULL,
			start_id TEXT NOT NULL DEFAULT '', last_sent_id TEXT NOT NULL DEFAULT '',
			updated_at INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id))`,
		`INSERT INTO anchors (workspace_id, group_id, start_id, last_sent_id, updated_at)
			VALUES ('qq_a', 'g1', '10', '20', 1761955200)`,
		`PRAGMA user_version = 9`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("造 v9 库失败：%v", err)
		}
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("升级后应能打开：%v", err)
	}
	defer st.Close()

	anchors, err := st.Scope("qq_a").LoadAnchors()
	if err != nil {
		t.Fatalf("LoadAnchors: %v", err)
	}
	got := anchors["g1"]
	if got.Start != "10" || got.LastSent != "20" {
		t.Fatalf("改名不能丢锚点：%+v", got)
	}
	if got.At.Unix() != 1761955200 {
		t.Fatalf("updated_at 应读回成 At（得到 %v）", got.At)
	}
}

// TestV8升级messages补自增主键 v8 的 messages 是三元组主键，升级要重建表：
// 消息一条不丢、按时间拿到递增 id，重建后还能接着写。
func TestV8升级messages补自增主键(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	statements := []string{
		`CREATE TABLE messages (
			workspace_id TEXT NOT NULL, group_id TEXT NOT NULL, message_id TEXT NOT NULL,
			user_id TEXT NOT NULL, content TEXT NOT NULL, images TEXT NOT NULL DEFAULT '[]',
			image_ids TEXT NOT NULL DEFAULT '[]', reply_to TEXT NOT NULL DEFAULT '',
			ref_idx TEXT NOT NULL DEFAULT '', ts INTEGER NOT NULL,
			PRIMARY KEY (workspace_id, group_id, message_id))`,
		`INSERT INTO messages (workspace_id, group_id, message_id, user_id, content, ts)
			VALUES ('qq_a', 'g1', 'm1', 'u1', '旧消息一', 1)`,
		`INSERT INTO messages (workspace_id, group_id, message_id, user_id, content, ts)
			VALUES ('qq_a', 'g1', 'm2', 'u1', '旧消息二', 2)`,
		`PRAGMA user_version = 8`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("造 v8 库失败：%v", err)
		}
	}
	_ = db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatalf("升级后应能打开：%v", err)
	}
	defer st.Close()
	scope := st.Scope("qq_a")
	msgs, err := scope.LoadRecent(10)
	if err != nil {
		t.Fatalf("LoadRecent: %v", err)
	}
	if len(msgs) != 2 || msgs[0].Content != "旧消息一" || msgs[1].Content != "旧消息二" {
		t.Fatalf("升级不能丢消息：%+v", msgs)
	}
	if msgs[0].RowID <= 0 || msgs[1].RowID <= msgs[0].RowID {
		t.Fatalf("按时间应拿到递增 id：%d / %d", msgs[0].RowID, msgs[1].RowID)
	}

	// 重建后还能接着写，行号要比历史的大
	if next := scope.SaveMessage(conversation.Message{MessageID: "m3", GroupID: "g1", Content: "新消息", Time: 3}); next <= msgs[1].RowID {
		t.Fatalf("重建后新写入的行号应大于历史行号 %d，实际 %d", msgs[1].RowID, next)
	}
}

// Test图片描述全局读写 image_notes 不带 workspace_id：任意工作空间都命中同一张图；
// 同哈希不同大小不算命中。
func Test图片描述全局读写(t *testing.T) {
	st, _ := openTemp(t)
	id := conversation.ImageID{SHA: "abc", Size: 123}
	note := conversation.ImageNote{
		ImageID: id, MIME: "image/png", Width: 10, Height: 20, Animated: false,
		Note: "新闻截图，标题关于天气", InfoLevel: "high", HasText: true,
		Status: "ok", Attempts: 1, Model: "m",
	}
	if err := st.SaveImageNote(note); err != nil {
		t.Fatalf("SaveImageNote: %v", err)
	}
	got, err := st.GetImageNotes([]conversation.ImageID{id, {SHA: "nope", Size: 1}})
	if err != nil {
		t.Fatalf("GetImageNotes: %v", err)
	}
	if len(got) != 1 || got[id].Note != note.Note || !got[id].HasText || got[id].InfoLevel != "high" {
		t.Fatalf("读取结果不对：%+v", got)
	}

	// 全局：Store 级读取（不带 workspace_id），任何工作空间都命中同一张图。
	if notes, err := st.GetImageNotes([]conversation.ImageID{id}); err != nil || len(notes) != 1 {
		t.Fatalf("全局图库应跨工作空间命中：%+v, %v", notes, err)
	}

	// 同哈希不同大小：不算命中。
	if notes, err := st.GetImageNotes([]conversation.ImageID{{SHA: "abc", Size: 999}}); err != nil || len(notes) != 0 {
		t.Fatalf("同哈希不同大小不应命中：%+v, %v", notes, err)
	}
}

// Test图片描述失败标记 失败写入累加 attempts，且保留已有尺寸信息。
func Test图片描述失败标记(t *testing.T) {
	st, _ := openTemp(t)
	id := conversation.ImageID{SHA: "s1", Size: 10}
	if err := st.SaveImageNote(conversation.ImageNote{ImageID: id, Width: 100, Height: 200, Status: "ok", Note: "梗图", InfoLevel: "low"}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkImageNoteFailed(conversation.ImageNote{ImageID: id, Width: 100, Height: 200, Attempts: 2}); err != nil {
		t.Fatalf("MarkImageNoteFailed: %v", err)
	}
	got, err := st.GetImageNotes([]conversation.ImageID{id})
	if err != nil || len(got) != 1 {
		t.Fatalf("读取失败记录：%+v, %v", got, err)
	}
	if got[id].Status != "failed" || got[id].Attempts != 2 || got[id].Width != 100 || got[id].Height != 200 {
		t.Fatalf("失败记录不对：%+v", got[id])
	}
}

// Test消息图片标识持久化 ImageIDs 随消息落库；LoadRecent 按 id 回填描述。
func Test消息图片标识持久化(t *testing.T) {
	st, scope := openTemp(t)
	id := conversation.ImageID{SHA: "s9", Size: 42}
	if err := st.SaveImageNote(conversation.ImageNote{ImageID: id, Note: "表情包", InfoLevel: "none", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
	scope.SaveMessage(conversation.Message{
		MessageID: "m1", GroupID: "g1", Content: "[图片]", Time: 1,
		Images: []string{"p1"}, ImageIDs: []conversation.ImageID{id},
	})
	waitQueue(t, st)

	msgs, err := scope.LoadRecent(1)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("LoadRecent: %+v, %v", msgs, err)
	}
	if len(msgs[0].ImageIDs) != 1 || msgs[0].ImageIDs[0] != id {
		t.Fatalf("image_ids 未持久化：%+v", msgs[0].ImageIDs)
	}
	if len(msgs[0].ImageNotes) != 1 || msgs[0].ImageNotes[0].Note != "表情包" {
		t.Fatalf("描述未按 id 回填：%+v", msgs[0].ImageNotes)
	}
}

// TestV4升级补图片字段 v4 库升级到 v5：补 image_ids 列与 image_notes 表，消息不丢。
func TestV4升级补图片字段(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	st, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := st.Scope("qq_a")
	scope.SaveMessage(conversation.Message{MessageID: "m1", GroupID: "g1", Content: "旧消息", Time: 1})
	waitQueue(t, st)
	// 退回 v4：去掉 image_ids 列与图库表。
	execDB(t, st, `ALTER TABLE messages DROP COLUMN image_ids`)
	execDB(t, st, `DROP TABLE image_notes`)
	execDB(t, st, `PRAGMA user_version = 4`)
	_ = st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatalf("升级后应能打开：%v", err)
	}
	defer st.Close()
	scope = st.Scope("qq_a")
	msgs, err := scope.LoadRecent(10)
	if err != nil || len(msgs) != 1 || msgs[0].Content != "旧消息" {
		t.Fatalf("升级不能丢消息：%+v, %v", msgs, err)
	}
	id := conversation.ImageID{SHA: "s1", Size: 1}
	if err := st.SaveImageNote(conversation.ImageNote{ImageID: id, Note: "图", Status: "ok"}); err != nil {
		t.Fatalf("升级后应可写图库：%v", err)
	}
	scope.SaveMessage(conversation.Message{MessageID: "m2", GroupID: "g1", Content: "[图片]", ImageIDs: []conversation.ImageID{id}, Time: 2})
	waitQueue(t, st)
	msgs, err = scope.LoadRecent(1)
	if err != nil || len(msgs) != 1 || len(msgs[0].ImageIDs) != 1 {
		t.Fatalf("补列后应可写 image_ids：%+v, %v", msgs, err)
	}
}

// TestLookupMemberNames批量补名 按群批量查显示名：群名片优先，其次账号名；
// 只有 ID、没有名字的成员与别的群都不返回；超过单条 SQL 参数上限时分批查完。
func TestLookupMemberNames批量补名(t *testing.T) {
	_, scope := openTemp(t)
	scope.SaveMember("g1", "u1", "账号名", "群名片")
	scope.SaveMember("g1", "u2", "只有账号名", "")
	scope.SaveMember("g1", "u3", "", "")
	scope.SaveMember("g2", "u4", "别的群", "")

	got := scope.LookupMemberNames("g1", []string{"u1", "u2", "u3", "nope"})
	if got["u1"] != "群名片" {
		t.Fatalf("应优先用群名片，实际 %q", got["u1"])
	}
	if got["u2"] != "只有账号名" {
		t.Fatalf("没有群名片时应退回账号名，实际 %q", got["u2"])
	}
	if _, ok := got["u3"]; ok {
		t.Fatal("没有名字的成员不应返回")
	}
	if _, ok := got["nope"]; ok {
		t.Fatal("库里没有的 user_id 不应返回")
	}

	if other := scope.LookupMemberNames("g2", []string{"u1", "u4"}); len(other) != 1 || other["u4"] != "别的群" {
		t.Fatalf("成员名应按群隔离，实际 %v", other)
	}

	// 超过 400/批的分批查询：450 个都要查回来。
	ids := make([]string, 0, 450)
	for i := 0; i < 450; i++ {
		id := "bulk-" + string(rune('a'+i%26)) + string(rune('a'+i/26%26))
		scope.SaveMember("g1", id, "批量"+id, "")
		ids = append(ids, id)
	}
	if got := scope.LookupMemberNames("g1", ids); len(got) != 450 {
		t.Fatalf("分批查询应返回全部 450 个名字，实际 %d", len(got))
	}
}
