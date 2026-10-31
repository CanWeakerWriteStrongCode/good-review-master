package state

import (
	"path/filepath"
	"testing"
	"time"

	"good-review-master/cache"
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

func Test资产欠条生命周期(t *testing.T) {
	st, _ := openTemp(t)
	now := time.Now().Unix()
	job := AssetTagJob{
		WorkspaceID: "w1", GroupID: "g1", MessageID: "m1", AssetIndex: 0,
		Kind: "image", URL: "http://x", Status: "pending", CreatedAt: now,
	}
	if err := st.SaveAssetTagJob(job); err != nil {
		t.Fatal(err)
	}
	pending, err := st.PendingAssetTagJobs(10, 3)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	job.Status = "failed"
	job.Attempts = 1
	job.LastError = "boom"
	if err := st.UpdateAssetTagJob(job); err != nil {
		t.Fatal(err)
	}
	pending, _ = st.PendingAssetTagJobs(10, 3)
	if len(pending) != 1 || pending[0].Attempts != 1 || pending[0].Status != "failed" {
		t.Fatalf("更新后 = %+v", pending)
	}
	// 达到上限后不再被扫描。
	if pending, _ := st.PendingAssetTagJobs(10, 1); len(pending) != 0 {
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
	id := cache.ImageID{SHA: "s1", Size: 1}
	if err := st.SaveImageNote(cache.ImageNote{ImageID: id, Note: "图", Status: "ok"}); err != nil {
		t.Fatal(err)
	}
}
