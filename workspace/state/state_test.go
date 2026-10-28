package state

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"good-review-master/cache"
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

func waitQueue(t *testing.T, st *Store) {
	t.Helper()
	// 异步写是后台 goroutine；测试里用一个同步查询等它排空（单连接串行执行）。
	deadline := time.Now().Add(2 * time.Second)
	for len(st.queue) > 0 {
		if time.Now().After(deadline) {
			t.Fatal("写队列未排空")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// channel 空了不代表最后一条已执行，借一次同步查询等它完成。
	_, _ = st.Stats()
}

func Test消息持久化与按群截断(t *testing.T) {
	dir := t.TempDir()
	st, err := Open(filepath.Join(dir, FileName))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	scope := st.Scope("qq_a")
	for i := 1; i <= 5; i++ {
		scope.SaveMessage(cache.Message{
			MessageID: string(rune('0' + i)), GroupID: "g1", UserID: "u",
			Content: "消息", Images: []string{"https://example.com/a.png"},
			ReplyToID: "1", Time: int64(i),
		})
	}
	scope.SaveMessage(cache.Message{MessageID: "x", GroupID: "g2", UserID: "u", Content: "另一个群", Time: 9})
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
	a.SaveMessage(cache.Message{MessageID: "1", GroupID: "g1", Content: "from-a", Time: 1})
	b.SaveMessage(cache.Message{MessageID: "1", GroupID: "g1", Content: "from-b", Time: 2})
	waitQueue(t, st)

	msgsA, err := a.LoadRecent(10)
	if err != nil || len(msgsA) != 1 || msgsA[0].Content != "from-a" {
		t.Fatalf("A 读取到 %+v, %v", msgsA, err)
	}
	msgsB, err := b.LoadRecent(10)
	if err != nil || len(msgsB) != 1 || msgsB[0].Content != "from-b" {
		t.Fatalf("B 读取到 %+v, %v", msgsB, err)
	}

	a.SaveAnchor("g1", cache.LLMAnchor{Start: "a1"})
	b.SaveAnchor("g1", cache.LLMAnchor{Start: "b1"})
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

	scope.SaveAnchor("g1", cache.LLMAnchor{Start: "10", LastSent: "20"})
	scope.SaveIdentity("g1", "openid-a")
	scope.SaveMember("g1", "u1", "张三", "老张")
	scope.SaveGroupPersona("g1", "锐评下")
	scope.SetMeta("name", "小助理")
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
	identities, err := scope.LoadIdentities()
	if err != nil || identities["g1"] != "openid-a" {
		t.Fatalf("identities = %+v, %v", identities, err)
	}
	members, err := scope.LoadMembers()
	if err != nil || members["g1"]["u1"] != "老张" {
		t.Fatalf("members = %+v, %v", members, err)
	}
	personas, err := scope.LoadGroupPersonas()
	if err != nil || personas["g1"] != "锐评下" {
		t.Fatalf("personas = %+v, %v", personas, err)
	}
	if got := scope.Meta("name"); got != "小助理" {
		t.Fatalf("meta name = %q", got)
	}
	if got := st.Scope("qq_other").Meta("name"); got != "" {
		t.Fatalf("meta 不应跨工作空间泄漏，得到 %q", got)
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
	scope.SaveMessage(cache.Message{MessageID: "1", GroupID: "g"})
	scope.SaveAnchor("g", cache.LLMAnchor{Start: "1"})
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
	if _, err := st.db.Exec(`DROP TABLE messages`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TABLE messages (group_id TEXT, message_id TEXT, user_id TEXT, content TEXT, images TEXT, reply_to TEXT, ts INTEGER)`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	_ = st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatalf("迁移后应能打开：%v", err)
	}
	defer st.Close()
	scope := st.Scope("qq_a")
	scope.SaveMessage(cache.Message{MessageID: "1", GroupID: "g", Content: "ok", Time: 1})
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
	scope.SaveMessage(cache.Message{MessageID: "m1", GroupID: "g1", Content: "旧消息", Time: 1})
	waitQueue(t, st)

	// 手动退回 v2：没有 groups 表。
	if _, err := st.db.Exec(`DROP TABLE groups`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`PRAGMA user_version = 2`); err != nil {
		t.Fatal(err)
	}
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

func TestWorkspaces返回平台与账号(t *testing.T) {
	st, scope := openTemp(t)
	scope.SetMeta("platform", "qq")
	scope.SetMeta("account", "1906")
	other := st.Scope("qq_b")
	other.SetMeta("platform", "qq")
	other.SetMeta("account", "b")

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
	if got := byID["qq_test"]; got.Platform != "qq" || got.Account != "1906" {
		t.Fatalf("qq_test meta = %+v", got)
	}
}

// Test引用索引反查 被引消息存下自己的 ref_idx，新消息用 ref_msg_idx 反查 message_id。
func Test引用索引反查(t *testing.T) {
	st, scope := openTemp(t)
	scope.SaveMessage(cache.Message{
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
	scope.SaveMessage(cache.Message{MessageID: "m2", GroupID: "g1", Content: "新消息", RefIdx: "REFIDX_N", Time: 2})
	waitQueue(t, st)
	if got := scope.ResolveMessageID("g1", "REFIDX_N"); got != "m2" {
		t.Fatalf("补列后应可写 ref_idx：%q", got)
	}
}
