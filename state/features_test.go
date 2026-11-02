package state

import (
	"path/filepath"
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

func TestV5升级迁移meta到画像(t *testing.T) {
	path := filepath.Join(t.TempDir(), FileName)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	execDB(t, st, `DROP TABLE workspace_profile`)
	execDB(t, st, `CREATE TABLE meta (workspace_id TEXT NOT NULL, key TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY (workspace_id, key))`)
	execDB(t, st, `INSERT INTO meta (workspace_id, key, value) VALUES
		('qq_official_bot_1906','platform','qq'),
		('qq_official_bot_1906','account_kind','official_bot'),
		('qq_official_bot_1906','account','1906'),
		('qq_official_bot_1906','name','小助理')`)
	execDB(t, st, `PRAGMA user_version = 5`)
	_ = st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatalf("升级后应能打开：%v", err)
	}
	defer st.Close()
	metas, err := st.Workspaces()
	if err != nil || len(metas) != 1 {
		t.Fatalf("workspaces = %+v, %v", metas, err)
	}
	if metas[0].Platform != "qq" || metas[0].AccountKind != "official_bot" ||
		metas[0].Account != "1906" || metas[0].Name != "小助理" {
		t.Fatalf("迁移结果 = %+v", metas[0])
	}
	if tableExists(st.db, "meta") {
		t.Fatal("迁移后应删除旧 meta 表")
	}
	// 迁移后仍可正常写图片描述。
	id := conversation.ImageID{SHA: "s1", Size: 1}
	if err := st.SaveImageNote(conversation.ImageNote{ImageID: id, Note: "图", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
}
