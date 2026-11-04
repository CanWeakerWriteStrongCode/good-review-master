package state

import (
	"testing"
	"time"
)

// Test处理台账把重投变幂等 覆盖三件事：没记过不算处理过、记过算处理过、
// 同一个 item 记两次既不报错也不变成两行。
func Test处理台账把重投变幂等(t *testing.T) {
	st, scope := openTemp(t)
	const id = "agent|qq_test|g1|m1"

	if scope.Processed(id) {
		t.Fatal("没记过的消息不应算处理过")
	}

	scope.MarkProcessed(id, "agent")
	if !scope.Processed(id) {
		t.Fatal("记过之后应算处理过")
	}

	// 重投：再记一次不能报错，也不能变成两行。
	scope.MarkProcessed(id, "agent")
	if got := countRows(t, st, "processed_items"); got != 1 {
		t.Fatalf("同一条消息记两次应只有 1 行，实际 %d", got)
	}
}

// Test处理台账按域隔离 同一条消息在不同域分别记账：sync 记过不等于 agent 处理过。
// 这正是「同一个域重复跑」能被拦住、而「下游域第一次跑」不会被上游误伤的原因。
func Test处理台账按域隔离(t *testing.T) {
	_, scope := openTemp(t)

	scope.MarkProcessed("sync|qq_test|g1|m1", "sync")
	if scope.Processed("agent|qq_test|g1|m1") {
		t.Fatal("别的域记过不算这一个域处理过")
	}
	if !scope.Processed("sync|qq_test|g1|m1") {
		t.Fatal("记过的那一条应算处理过")
	}
}

// Test清理只删过期的台账 窗口之前的删掉，窗口之内的保留。
func Test清理只删过期的台账(t *testing.T) {
	st, scope := openTemp(t)

	scope.MarkProcessed("old", "agent")
	scope.MarkProcessed("fresh", "agent")
	if err := st.db.Model(&ProcessedItem{}).Where("item_id = ?", "old").
		Update("created_at", time.Now().Add(-24*time.Hour).Unix()).Error; err != nil {
		t.Fatalf("改台账时间失败：%v", err)
	}

	if n := st.PurgeProcessed(time.Now().Add(-time.Hour).Unix()); n != 1 {
		t.Fatalf("应删掉 1 条过期台账，实际 %d", n)
	}
	if scope.Processed("old") {
		t.Fatal("过期的台账应已删除")
	}
	if !scope.Processed("fresh") {
		t.Fatal("窗口内的台账不该被删")
	}
}

// Test处理台账的零值调用 没有库（纯内存运行）时不能 panic，也不能把消息当成处理过：
// 那种模式下没有重投风险，放行才是不丢消息的选择。
func Test处理台账的零值调用(t *testing.T) {
	var scope *Scope
	if scope.Processed("x") {
		t.Fatal("没有库时不应声称处理过")
	}
	scope.MarkProcessed("x", "agent") // 不能 panic
	var st *Store
	if n := st.PurgeProcessed(time.Now().Unix()); n != 0 {
		t.Fatalf("没有库时清理应返回 0，实际 %d", n)
	}
}
