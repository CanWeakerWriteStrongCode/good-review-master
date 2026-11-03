package state

import (
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"good-review-master/conversation"
)

func historyCount(t *testing.T, st *Store, table, workspaceID string) int {
	t.Helper()
	var count int
	if err := st.db.Raw(`SELECT COUNT(*) FROM `+table+` WHERE workspace_id = ?`, workspaceID).Scan(&count).Error; err != nil {
		t.Fatalf("统计 %s: %v", table, err)
	}
	return count
}

func Test成员历史只在信息变化时追加且按消息去重(t *testing.T) {
	st, scope := openTemp(t)
	scope.SaveMemberInfo(MemberInfo{
		GroupID: "g1", UserID: "u1", Name: "旧名", Source: "sender", SourceMessageID: "m1",
	})
	if got := historyCount(t, st, "member_info_history", scope.workspaceID); got != 1 {
		t.Fatalf("首次应写 1 条历史，实际 %d", got)
	}
	// 同一条消息重试：当前值没变，不追加。
	scope.SaveMemberInfo(MemberInfo{
		GroupID: "g1", UserID: "u1", Name: "旧名", Source: "sender", SourceMessageID: "m1",
	})
	if got := historyCount(t, st, "member_info_history", scope.workspaceID); got != 1 {
		t.Fatalf("重试不应追加历史，实际 %d", got)
	}
	// 改名：追加第二条，当前值更新。
	if changed, err := scope.SaveMemberInfo(MemberInfo{
		GroupID: "g1", UserID: "u1", Name: "新名", DisplayName: "群名片",
		Source: "sender", SourceMessageID: "m2",
	}); err != nil || !changed {
		t.Fatalf("改名应标记变化：%v, %v", changed, err)
	}
	if got := historyCount(t, st, "member_info_history", scope.workspaceID); got != 2 {
		t.Fatalf("改名后应有 2 条历史，实际 %d", got)
	}
	names := scope.LookupMemberNames("g1", []string{"u1"})
	if names["u1"] != "群名片" {
		t.Fatalf("当前值应为群名片，实际 %q", names["u1"])
	}
}

func Test群历史与当前值(t *testing.T) {
	st, scope := openTemp(t)
	scope.SaveGroupInfo("g1", "群A", "group_info", "m1", "{}")
	scope.SaveGroupInfo("g1", "群A", "group_info", "m2", "{}")
	if got := historyCount(t, st, "group_info_history", scope.workspaceID); got != 1 {
		t.Fatalf("同名不应追加历史，实际 %d", got)
	}
	scope.SaveGroupInfo("g1", "群B", "group_info", "m3", "{}")
	if got := historyCount(t, st, "group_info_history", scope.workspaceID); got != 2 {
		t.Fatalf("改名后应有 2 条历史，实际 %d", got)
	}
	if got := scope.GroupName("g1"); got != "群B" {
		t.Fatalf("群名 = %q", got)
	}
}

// Test群统计不再白烧自增号 消息、成员、群档案都是反复写入同一个群，
// groups.id 只能按群的数量增长。SQLite 的 upsert 即使只走更新分支也会分配
// 自增 rowid 并推进 sqlite_sequence，所以触发器和写入口都改成"先 UPDATE、
// 没命中才 INSERT"；这里用重复写入把这条约束钉住。
func Test群统计不再白烧自增号(t *testing.T) {
	st, scope := openTemp(t)
	for i := 0; i < 20; i++ {
		scope.SaveMessage(conversation.Message{
			GroupID: "g1", MessageID: "m" + strconv.Itoa(i), UserID: "u1",
			Content: "hi", Time: int64(100 + i),
		})
		scope.SaveMember("g1", "u1", "甲", "老甲")
		scope.SaveGroupInfo("g1", "群一", "group_info", "", "{}")
		scope.SaveGroupPersona("g1", "锐评下")
	}
	scope.SaveMessage(conversation.Message{
		GroupID: "g2", MessageID: "m-x", UserID: "u2", Content: "hi", Time: 300,
	})

	var rows []groupRow
	if err := st.db.Raw(`SELECT id, group_id, message_count, member_count FROM groups ORDER BY id`).
		Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].ID != 1 || rows[1].ID != 2 {
		t.Fatalf("groups.id 应跟着群数走，实际 %+v", rows)
	}
	if rows[0].MessageCount != 20 || rows[0].MemberCount != 1 {
		t.Fatalf("计数被改坏了：%+v", rows[0])
	}
	if rows[1].MessageCount != 1 {
		t.Fatalf("新群计数应为 1：%+v", rows[1])
	}
}

// Test重复写入不再白烧其余表的自增号 members / member_info_history / anchors /
// workspace_profile 都是反复命中同一行的写入，序列号只该跟着真实新行增长。
// 老写法用 upsert（含 ON CONFLICT DO NOTHING），SQLite 在冲突前就分配了 rowid，
// 就算最后只更新/被忽略，序列也白跑一格。
func Test重复写入不再白烧其余表的自增号(t *testing.T) {
	st, scope := openTemp(t)

	// 同一个成员反复改名：members 只有 1 行。
	for i := 0; i < 5; i++ {
		scope.SaveMemberInfo(MemberInfo{
			GroupID: "g1", UserID: "u1", Name: "名字" + strconv.Itoa(i),
			Source: "sender", SourceMessageID: "m" + strconv.Itoa(i),
		})
	}
	// 同一条消息重试、信息却不一样：历史按 source_message_id 去重，不该插第二行。
	scope.SaveMemberInfo(MemberInfo{
		GroupID: "g1", UserID: "u1", Name: "重试名", Source: "sender", SourceMessageID: "m0",
	})

	// 锚点反复推进、画像反复同步。
	for i := 0; i < 5; i++ {
		scope.SaveAnchor("g1", conversation.LLMAnchor{
			Start: strconv.Itoa(i), LastSent: strconv.Itoa(i + 1),
		})
	}
	for i := 0; i < 5; i++ {
		if err := scope.SaveProfile(WorkspaceProfile{Name: "画像" + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	scope.SaveSelfUserID("openid-a")
	scope.SaveSelfUserID("openid-a") // 值没变，什么都不该动

	want := map[string]int{
		"members": 1, "member_info_history": 5, "anchors": 1, "workspace_profile": 1,
	}
	for table, wantRows := range want {
		var rows, seq int
		if err := st.db.Raw(`SELECT COUNT(*) FROM ` + table).Scan(&rows).Error; err != nil {
			t.Fatalf("统计 %s: %v", table, err)
		}
		if err := st.db.Raw(`SELECT seq FROM sqlite_sequence WHERE name = ?`, table).Scan(&seq).Error; err != nil {
			t.Fatalf("读 %s 序列: %v", table, err)
		}
		if rows != wantRows {
			t.Fatalf("%s 应有 %d 行，实际 %d", table, wantRows, rows)
		}
		if seq != wantRows {
			t.Fatalf("%s 序列应等于行数 %d（没有空烧），实际 %d", table, wantRows, seq)
		}
	}
}

// Test单行写入不再白烧自增号 messages / send_ledger / settings / asset_tag_jobs
// 都是"业务键唯一、反复重写同一行"的表，序列号只该跟着真实新行增长。
func Test单行写入不再白烧自增号(t *testing.T) {
	st, scope := openTemp(t)

	for i := 0; i < 5; i++ {
		scope.SaveMessage(conversation.Message{
			GroupID: "g1", MessageID: "m1", UserID: "u1",
			Content: "第" + strconv.Itoa(i) + "次", Time: int64(i),
		})
	}
	for i := 0; i < 5; i++ {
		scope.MarkSend(SendRecord{
			ActionID: "a1", MessageID: "bot-1", Text: "第" + strconv.Itoa(i) + "次", Sent: true,
		})
	}
	for i := 0; i < 5; i++ {
		if err := st.SaveSetting("window", i); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := st.SaveAssetTagJob(AssetTagJob{
			WorkspaceID: "w1", GroupID: "g1", MessageID: "m1", AssetIndex: 0, Status: "pending",
		}); err != nil {
			t.Fatal(err)
		}
	}

	want := map[string]int{"messages": 1, "send_ledger": 1, "settings": 1, "asset_tag_jobs": 1}
	for table, wantRows := range want {
		var rows, seq int
		if err := st.db.Raw(`SELECT COUNT(*) FROM ` + table).Scan(&rows).Error; err != nil {
			t.Fatalf("统计 %s: %v", table, err)
		}
		if err := st.db.Raw(`SELECT seq FROM sqlite_sequence WHERE name = ?`, table).Scan(&seq).Error; err != nil {
			t.Fatalf("读 %s 序列: %v", table, err)
		}
		if rows != wantRows {
			t.Fatalf("%s 应有 %d 行，实际 %d", table, wantRows, rows)
		}
		if seq != wantRows {
			t.Fatalf("%s 序列应等于行数 %d（没有空烧），实际 %d", table, wantRows, seq)
		}
	}
}

func Test资产欠条领取与租约(t *testing.T) {
	st, _ := openTemp(t)
	now := time.Now()
	job := AssetTagJob{
		WorkspaceID: "w1", GroupID: "g1", MessageID: "m1", AssetIndex: 0,
		Kind: "image", URL: "http://x", Status: "pending", CreatedAt: now.Unix(),
	}
	if err := st.SaveAssetTagJob(job); err != nil {
		t.Fatal(err)
	}
	reload := func() AssetTagJob {
		var got AssetTagJob
		if err := st.db.First(&got, "workspace_id = ? AND group_id = ? AND message_id = ? AND asset_index = ?",
			"w1", "g1", "m1", 0).Error; err != nil {
			t.Fatal(err)
		}
		return got
	}
	claim := AssetTagClaim{Now: now, StaleBefore: now.Add(-time.Minute), MaxAttempts: 3}

	candidates, err := st.ClaimableAssetTagJobs(10, claim)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("候选 = %+v, %v", candidates, err)
	}
	if ok, err := st.ClaimAssetTagJob(job, claim); err != nil || !ok {
		t.Fatalf("刚建好的欠条应该能抢到：%v, %v", ok, err)
	}
	// 同一条不能抢两次：这一次的 WHERE 已经对不上了。
	if ok, _ := st.ClaimAssetTagJob(job, claim); ok {
		t.Fatal("已经被领走的欠条不应再抢到")
	}
	// attempts 在抢到的那一刻就加了，不等到回写结果。
	if got := reload(); got.Status != "running" || got.Attempts != 1 {
		t.Fatalf("领走后 = %+v，期望 running / attempts 1", got)
	}
	// 租约没过期：running 不该再出现在候选里。
	if again, _ := st.ClaimableAssetTagJobs(10, claim); len(again) != 0 {
		t.Fatalf("租约内的 running 不该是候选：%+v", again)
	}

	// 10 分钟后（租约 1 分钟）视为领走它的进程已经没了，可以重新领。
	later := now.Add(10 * time.Minute)
	stale := AssetTagClaim{Now: later, StaleBefore: later.Add(-time.Minute), MaxAttempts: 3}
	if again, _ := st.ClaimableAssetTagJobs(10, stale); len(again) != 1 {
		t.Fatalf("租约过期的 running 应重新成为候选：%+v", again)
	}
	if ok, err := st.ClaimAssetTagJob(job, stale); err != nil || !ok {
		t.Fatalf("租约过期后应能重新领：%v, %v", ok, err)
	}

	// 回写终态：attempts 由领取负责，这里填一个错值也不该被写进库。
	got := reload()
	got.Status = "failed"
	got.LastError = "boom"
	got.Attempts = 99
	if err := st.UpdateAssetTagJob(got); err != nil {
		t.Fatal(err)
	}
	if stored := reload(); stored.Status != "failed" || stored.LastError != "boom" || stored.Attempts != 2 {
		t.Fatalf("回写后 = %+v，期望 failed / boom / attempts 2", stored)
	}
	// 达到上限后不再被扫。
	if pending, _ := st.ClaimableAssetTagJobs(10, AssetTagClaim{Now: later, StaleBefore: later, MaxAttempts: 2}); len(pending) != 0 {
		t.Fatalf("达到上限后不应返回：%+v", pending)
	}

	if err := st.db.Exec(`UPDATE asset_tag_jobs SET status = 'done', updated_at = ?`,
		time.Now().Add(-48*time.Hour).Unix()).Error; err != nil {
		t.Fatal(err)
	}
	removed, err := st.CleanupAssetTagJobs(time.Now(), 24*time.Hour)
	if err != nil || removed != 1 {
		t.Fatalf("清理 = %d, %v", removed, err)
	}
}

// Test同一欠条并发只有一个进程抢到 守的是多副本的底线：
// 条件 UPDATE 必须原子，否则两个进程会把同一张图交给模型描述两次，白花钱。
func Test同一欠条并发只有一个进程抢到(t *testing.T) {
	st, _ := openTemp(t)
	now := time.Now()
	job := AssetTagJob{
		WorkspaceID: "w1", GroupID: "g1", MessageID: "m1", AssetIndex: 0,
		Kind: "image", URL: "http://x", Status: "pending", CreatedAt: now.Unix(),
	}
	if err := st.SaveAssetTagJob(job); err != nil {
		t.Fatal(err)
	}
	// 上限调高，避免 attempts 先被抢完干扰结论。
	claim := AssetTagClaim{Now: now, StaleBefore: now.Add(-time.Minute), MaxAttempts: 100}

	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if ok, err := st.ClaimAssetTagJob(job, claim); err == nil && ok {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("16 个协程抢同一条，应只有 1 个抢到，实际 %d", wins)
	}
}

// Test两个连接抢同一批欠条每条只被领一次 模拟两个进程：它们是两个独立的数据库连接，
// 走的是 WAL + busy_timeout 那条真实的竞争路径（单连接下测不到这个）。
func Test两个连接抢同一批欠条每条只被领一次(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = second.Close() }()

	const count = 20
	now := time.Now()
	for i := 0; i < count; i++ {
		job := AssetTagJob{
			WorkspaceID: "w1", GroupID: "g1", MessageID: "m1", AssetIndex: i,
			Kind: "image", URL: "http://x", Status: "pending", CreatedAt: now.Unix(),
		}
		if err := first.SaveAssetTagJob(job); err != nil {
			t.Fatal(err)
		}
	}
	claim := AssetTagClaim{Now: now, StaleBefore: now.Add(-time.Minute), MaxAttempts: 100}

	wins := make([]int32, count)
	var wg sync.WaitGroup
	for _, st := range []*Store{first, second} {
		wg.Add(1)
		go func(st *Store) {
			defer wg.Done()
			candidates, err := st.ClaimableAssetTagJobs(count, claim)
			if err != nil {
				return
			}
			for _, job := range candidates {
				if ok, err := st.ClaimAssetTagJob(job, claim); err == nil && ok {
					atomic.AddInt32(&wins[job.AssetIndex], 1)
				}
			}
		}(st)
	}
	wg.Wait()

	for i, got := range wins {
		if got != 1 {
			t.Fatalf("第 %d 条被领了 %d 次，应该正好 1 次", i, got)
		}
	}
}

func Test发送台账与全局设置(t *testing.T) {
	st, scope := openTemp(t)
	if _, ok, _ := scope.LoadSendRecord("a1"); ok {
		t.Fatal("初始不应有台账")
	}
	scope.MarkSend(SendRecord{ActionID: "a1", MessageID: "m1", Text: "一", Sent: true, SendOK: false})
	record, ok, err := scope.LoadSendRecord("a1")
	if err != nil || !ok || !record.Sent || record.SendOK {
		t.Fatalf("台账 = %+v, %v, %v", record, ok, err)
	}
	if err := st.SaveSetting("pipeline", map[string]any{"sync_batch": 7}); err != nil {
		t.Fatal(err)
	}
	settings, err := st.LoadSettings()
	if err != nil || len(settings) != 1 {
		t.Fatalf("settings = %+v, %v", settings, err)
	}
}

func Test画像三元组唯一(t *testing.T) {
	st, _ := openTemp(t)
	if err := st.Scope("w1").SaveProfile(WorkspaceProfile{
		Platform: "qq", AccountKind: "official_bot", Account: "1906",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Scope("w2").SaveProfile(WorkspaceProfile{
		Platform: "qq", AccountKind: "official_bot", Account: "1906",
	}); err == nil {
		t.Fatal("同一三元组写两个工作空间应被唯一索引拒绝")
	}
	// 同平台不同账号类型是合法的。
	if err := st.Scope("w3").SaveProfile(WorkspaceProfile{
		Platform: "qq", AccountKind: "personal_account", Account: "1906",
	}); err != nil {
		t.Fatalf("不同账号类型不应冲突：%v", err)
	}
}

// Test消息占位待办 落库时就定下“这条 @ 了谁”和“正文里还有没有占位”，
// 回填只改正文与待办标记；where 带旧正文是为了多副本同时改同一行时不互相覆盖。
func Test消息占位待办(t *testing.T) {
	_, scope := openTemp(t)
	const (
		rice = "ADA3988201B97492C659ADDAB0EAF113"
		fish = "DBF09B44D411F63340CE2924D744BA86"
	)
	placeholder := "@用户(" + rice + ") 你好啊"
	rowID := scope.SaveMessage(conversation.Message{
		MessageID: "m1", GroupID: "g1", UserID: "u1", Content: placeholder,
		Mentions: []string{rice}, Time: 100,
	})
	if rowID == 0 {
		t.Fatal("消息应落库并拿到行号")
	}
	// 没 @ 人的消息不进待办；另一个群的行不能串进来。
	if got := scope.SaveMessage(conversation.Message{MessageID: "m2", GroupID: "g1", UserID: "u1", Content: "今天好热", Time: 101}); got == 0 {
		t.Fatal("第二条消息应落库")
	}
	if got := scope.SaveMessage(conversation.Message{
		MessageID: "m3", GroupID: "g2", UserID: "u1",
		Content: "@用户(" + fish + ") 在吗", Mentions: []string{fish}, Time: 102,
	}); got == 0 {
		t.Fatal("另一个群的消息应落库")
	}

	rows, err := scope.PendingMentionRows("g1", 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("g1 的待办 = %+v, %v", rows, err)
	}
	if rows[0].ID != rowID || rows[0].Content != placeholder {
		t.Fatalf("待办行 = %+v", rows[0])
	}
	groups, err := scope.PendingMentionGroups()
	if err != nil || len(groups) != 2 {
		t.Fatalf("两个群都有待办，实际 %v, %v", groups, err)
	}

	// 旧正文对不上（别人刚改过）就不写，等下一轮重读。
	if ok, err := scope.RewriteMention(rowID, "@用户("+rice+") 你好", "@谁是大肥鱼("+rice+") 你好", 1); err != nil || ok {
		t.Fatalf("旧正文对不上时应改不到：%v, %v", ok, err)
	}
	replaced := "@谁是大肥鱼(" + rice + ") 你好啊"
	if ok, err := scope.RewriteMention(rowID, placeholder, replaced, 1); err != nil || !ok {
		t.Fatalf("回填应命中一行：%v, %v", ok, err)
	}
	if rows, err := scope.PendingMentionRows("g1", 10); err != nil || len(rows) != 0 {
		t.Fatalf("回填后应出列，实际 %+v, %v", rows, err)
	}

	// 重启后读回来的正文与 @ 名单都是新的。
	msgs, err := scope.LoadRecent(10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, msg := range msgs {
		if msg.MessageID != "m1" {
			continue
		}
		found = true
		if msg.Content != replaced {
			t.Fatalf("正文应为回填后的，实际 %q", msg.Content)
		}
		if len(msg.Mentions) != 1 || msg.Mentions[0] != rice {
			t.Fatalf("mentions 应读回 [%s]，实际 %v", rice, msg.Mentions)
		}
	}
	if !found {
		t.Fatal("LoadRecent 应读到 m1")
	}
}

// MemberCount 读的是 groups.member_count（成员表插入时触发器累加），
// 和面板群列表同一个口径：一个人只算一次，没记录的会话是 0。
func TestMemberCount读已收录人数(t *testing.T) {
	_, scope := openTemp(t)

	// 先有消息：触发器建出 groups 行，此时成员数是 0。
	scope.SaveMessage(conversation.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Content: "hi", Time: 100})
	if got := scope.MemberCount("g1"); got != 0 {
		t.Fatalf("只有消息时成员数应为 0，实际 %d", got)
	}

	for _, userID := range []string{"u1", "u1", "u2"} {
		if _, err := scope.SaveMemberInfo(MemberInfo{GroupID: "g1", UserID: userID, Name: "群友"}); err != nil {
			t.Fatal(err)
		}
	}
	// u1 出现两次（UPSERT 走更新分支，不触发累加）：只算 1 个人。
	if got := scope.MemberCount("g1"); got != 2 {
		t.Fatalf("成员数 = %d, want 2", got)
	}

	// 没进过库的会话不该凭空给出数字。
	if got := scope.MemberCount("g404"); got != 0 {
		t.Fatalf("未知会话的成员数 = %d, want 0", got)
	}
}
