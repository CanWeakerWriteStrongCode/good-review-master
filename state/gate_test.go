package state

import "testing"

// Test会话开关三态 锁住「键在不在」这套语义：
// 没写过 = 交给 bot.agent_default_enabled；写过 false = 用户明确关过。
// 两者读出来都是 false，只能靠"键在不在"区分，所以这里逐个断言存在性。
func Test会话开关三态(t *testing.T) {
	st, scope := openTemp(t)

	gates, err := scope.LoadGroupGates()
	if err != nil {
		t.Fatalf("LoadGroupGates: %v", err)
	}
	if len(gates) != 0 {
		t.Fatalf("一条都没设置过时应返回空 map，实际 %+v", gates)
	}

	if err := scope.SaveGroupGate("g1", false); err != nil {
		t.Fatalf("SaveGroupGate(g1, false): %v", err)
	}
	if err := scope.SaveGroupGate("g2", true); err != nil {
		t.Fatalf("SaveGroupGate(g2, true): %v", err)
	}

	gates, err = scope.LoadGroupGates()
	if err != nil {
		t.Fatalf("LoadGroupGates: %v", err)
	}
	if enabled, ok := gates["g1"]; !ok || enabled {
		t.Fatalf("g1 应当显式关着，ok=%v enabled=%v", ok, enabled)
	}
	if enabled, ok := gates["g2"]; !ok || !enabled {
		t.Fatalf("g2 应当显式开着，ok=%v enabled=%v", ok, enabled)
	}
	// g3 从未设置：不能因为 g1/g2 存在就被顺带塞一个默认值进来。
	if _, ok := gates["g3"]; ok {
		t.Fatal("没设置过的会话不应出现在结果里")
	}

	// 同一个键重复写：只更新值，不能变成两行。
	if err := scope.SaveGroupGate("g1", true); err != nil {
		t.Fatalf("SaveGroupGate(g1, true): %v", err)
	}
	gates, err = scope.LoadGroupGates()
	if err != nil {
		t.Fatalf("LoadGroupGates: %v", err)
	}
	if !gates["g1"] {
		t.Fatalf("覆盖写之后 g1 应当开着，实际 %+v", gates)
	}
	if len(gates) != 2 {
		t.Fatalf("覆盖写不应新增行，实际 %+v", gates)
	}

	// 别的账号的开关不能读进来：同一个库里可能挂着多个工作空间。
	other, err := st.Scope("qq_other").LoadGroupGates()
	if err != nil {
		t.Fatalf("LoadGroupGates(other): %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("会话开关不应跨工作空间泄漏，实际 %+v", other)
	}
}

// Test会话开关持久化 换一个连接重新打开：开关是群的状态，不能只活在内存里。
func Test会话开关持久化(t *testing.T) {
	st, scope := openTemp(t)
	if err := scope.SaveGroupGate("g1", true); err != nil {
		t.Fatalf("SaveGroupGate: %v", err)
	}
	path := st.path
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	gates, err := reopened.Scope("qq_test").LoadGroupGates()
	if err != nil {
		t.Fatalf("LoadGroupGates: %v", err)
	}
	if !gates["g1"] {
		t.Fatalf("重启后 g1 应当仍然开着，实际 %+v", gates)
	}
}

// Test会话开关空键无副作用 会话标识为空时静默跳过：
// 状态为 nil / 已关库时同样不 panic，让调用方不必到处判空。
func Test会话开关空键无副作用(t *testing.T) {
	_, scope := openTemp(t)

	if err := scope.SaveGroupGate("", true); err != nil {
		t.Fatalf("空会话标识不应报错: %v", err)
	}
	gates, err := scope.LoadGroupGates()
	if err != nil {
		t.Fatalf("LoadGroupGates: %v", err)
	}
	if len(gates) != 0 {
		t.Fatalf("空会话标识不应写进库，实际 %+v", gates)
	}

	var nilScope *Scope
	if err := nilScope.SaveGroupGate("g1", true); err != nil {
		t.Fatalf("nil Scope 不应报错: %v", err)
	}
	if gates, err := nilScope.LoadGroupGates(); err != nil || gates != nil {
		t.Fatalf("nil Scope 应返回 (nil, nil)，实际 (%+v, %v)", gates, err)
	}
}
