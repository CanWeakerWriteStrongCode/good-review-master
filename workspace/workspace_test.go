package workspace

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/logutil"
)

// TestMain 把 cwd 切到临时目录：logutil 默认把日志写到 cwd/log，
// 不隔离就会在仓库里留下 workspace/log/bot.log。
func TestMain(m *testing.M) {
	original, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	temp, err := os.MkdirTemp("", "workspace-test-")
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

func testManager(t *testing.T, cfg *config.Config) *Manager {
	t.Helper()
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manager, err := New(func() *config.Config { return cfg }, system,
		filepath.Join(dir, "prompt_custom.yaml"), nil, nil, nil, context.Background())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(manager.Close)
	return manager
}

func allowedConfig(accounts ...string) *config.Config {
	return &config.Config{AllowGroups: []string{"g1"}, WorkspaceAccounts: accounts}
}

func TestWorkspaceID(t *testing.T) {
	cases := []struct {
		platform, account, want string
		wantErr                 bool
	}{
		{"qq", "1906xxxx", "qq_1906xxxx", false},
		{"zulip.my-instance", "abc", "zulip_my-instance_abc", false},
		{"qq_official", "1", "", true}, // 下划线不符合 OB12 平台名
		{"qq", "bad/account", "", true},
		{"qq", "", "", true},
	}
	for _, tc := range cases {
		got, err := WorkspaceID(tc.platform, tc.account)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("WorkspaceID(%q,%q) 应失败，得到 %q", tc.platform, tc.account, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("WorkspaceID(%q,%q) = %q, %v; want %q", tc.platform, tc.account, got, err, tc.want)
		}
	}
}

func TestResolveOrCreate按名单开通(t *testing.T) {
	m := testManager(t, allowedConfig("qq_1906xxxx"))

	ws, err := m.ResolveOrCreate("qq", "1906xxxx")
	if err != nil || ws == nil {
		t.Fatalf("ResolveOrCreate = %v, %v", ws, err)
	}
	if ws.ID != "qq_1906xxxx" || ws.Platform != "qq" || ws.Account != "1906xxxx" {
		t.Fatalf("workspace = %+v", ws)
	}
	if ws.State == nil {
		t.Fatal("工作空间应挂上 state.db 的 Scope")
	}

	again, err := m.ResolveOrCreate("qq", "1906xxxx")
	if err != nil || again != ws {
		t.Fatalf("同一账号应复用同一个工作空间")
	}

	denied, err := m.ResolveOrCreate("qq", "9999")
	if err != nil || denied != nil {
		t.Fatalf("不在名单里的账号应返回 nil（不接管），得到 %v, %v", denied, err)
	}
}

// Test档案里的工作空间无需名单 锁住 D4：名单只控制新建，
// 重启后已有工作空间从 state.db 恢复，删掉名单不会让它停止工作。
func Test档案里的工作空间无需名单(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	newManager := func(cfg *config.Config) *Manager {
		m, err := New(func() *config.Config { return cfg }, system,
			filepath.Join(dir, "prompt_custom.yaml"), nil, nil, nil, context.Background())
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		return m
	}

	first := newManager(allowedConfig("qq_existing"))
	if ws, err := first.ResolveOrCreate("qq", "existing"); err != nil || ws == nil {
		t.Fatalf("首次开通失败：%v, %v", ws, err)
	}
	first.Close()

	// 重启时名单为空：工作空间已在档案里，仍应被加载
	restarted := newManager(allowedConfig())
	t.Cleanup(restarted.Close)
	if _, ok := restarted.ByID("qq_existing"); !ok {
		t.Fatalf("档案里的工作空间应被恢复，实际 %+v", restarted.List())
	}
}

func Test启动恢复时跳过不完整档案(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := New(func() *config.Config { return &config.Config{} }, system,
		filepath.Join(dir, "prompt_custom.yaml"), nil, nil, nil, context.Background())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	// 只有 platform、没有 account：无法重建工作空间，应跳过而不是报错退出
	first.stateStore.Scope("qq_bad").SetMeta("platform", "qq")
	first.stateStore.Scope("not-a-workspace!").SetMeta("platform", "qq")
	first.Close()

	m, err := New(func() *config.Config { return &config.Config{} }, system,
		filepath.Join(dir, "prompt_custom.yaml"), nil, nil, nil, context.Background())
	if err != nil {
		t.Fatalf("不完整档案不该让启动失败：%v", err)
	}
	t.Cleanup(m.Close)
	if len(m.List()) != 0 {
		t.Fatalf("不完整档案应被跳过，实际 %+v", m.List())
	}
}

func Test并发开通只创建一次(t *testing.T) {
	m := testManager(t, allowedConfig("qq_1"))

	var wg sync.WaitGroup
	results := make([]*Workspace, 16)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ws, err := m.ResolveOrCreate("qq", "1")
			if err != nil {
				t.Errorf("ResolveOrCreate: %v", err)
			}
			results[i] = ws
		}(i)
	}
	wg.Wait()
	for i := 1; i < len(results); i++ {
		if results[i] != results[0] {
			t.Fatalf("并发开通应只创建一个实例")
		}
	}
}

func Test缓存隔离而提示词共享(t *testing.T) {
	m := testManager(t, allowedConfig("qq_a", "qq_b"))
	wsA, _ := m.ResolveOrCreate("qq", "a")
	wsB, _ := m.ResolveOrCreate("qq", "b")

	wsA.Cache.GetGroupCache("g1", 8).Add(cache.Message{MessageID: "1", Content: "a", Time: 1})
	wsB.Cache.GetGroupCache("g1", 8).Add(cache.Message{MessageID: "1", Content: "b", Time: 1})
	if wsA.Cache.GetGroupCache("g1", 8).GetAll()[0].Content != "a" {
		t.Fatal("账号 A 的缓存被账号 B 污染")
	}

	// 提示词只有一份：任何账号的增删都写根目录的 prompt_custom.yaml。
	if wsA.Prompt != wsB.Prompt {
		t.Fatal("所有工作空间应共享同一个提示词配置")
	}
	if err := wsA.Prompt.AddCommand("chat_review", "共享指令", "提示词", nil); err != nil {
		t.Fatalf("AddCommand() error = %v", err)
	}
	wsB.Prompt.Reload()
	if len(wsB.Prompt.Snapshot().CmdConfigs["chat_review"]) != 1 {
		t.Fatal("账号 B 应看到共享提示词里新增的指令")
	}
}

func TestManager识别昵称唯一性(t *testing.T) {
	m := testManager(t, allowedConfig("qq_a", "qq_b"))
	wsA, _ := m.ResolveOrCreate("qq", "a")
	wsB, _ := m.ResolveOrCreate("qq", "b")

	wsA.setName("同名")
	wsB.setName("同名")
	if m.NicknameUnique("同名") {
		t.Fatal("两个账号同名时不能用昵称做归属兜底")
	}
	wsB.setName("另一个")
	if !m.NicknameUnique("另一个") {
		t.Fatal("昵称唯一时应允许兜底")
	}
	if m.NicknameUnique("") {
		t.Fatal("空昵称不能做兜底")
	}
}

func TestDefault只在单工作空间时返回(t *testing.T) {
	m := testManager(t, allowedConfig("qq_a", "qq_b"))
	if m.Default() != nil {
		t.Fatal("没有工作空间时 Default 应为 nil")
	}
	wsA, _ := m.ResolveOrCreate("qq", "a")
	if m.Default() != wsA {
		t.Fatal("只有一个工作空间时应返回它")
	}
	m.ResolveOrCreate("qq", "b")
	if m.Default() != nil {
		t.Fatal("多个工作空间时不能猜默认账号")
	}
}

// Test多账号共用一个档案库 锁住"单库 + workspace_id"的语义：
// 库文件在工作目录而不是每个工作空间一个库；两个账号写同一个群、同一个 message_id
// 互不覆盖，重启后各自恢复自己的内容。
func Test多账号共用一个档案库(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		AllowGroups:       []string{"g1"},
		WorkspaceAccounts: []string{"qq_a", "qq_b"},
		MaxCacheMsg:       10,
	}

	m, err := New(func() *config.Config { return cfg }, system,
		filepath.Join(dir, "prompt_custom.yaml"), nil, nil, nil, context.Background())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	wsA, err := m.ResolveOrCreate("qq", "a")
	if err != nil || wsA == nil {
		t.Fatalf("ResolveOrCreate(a) = %v, %v", wsA, err)
	}
	wsB, err := m.ResolveOrCreate("qq", "b")
	if err != nil || wsB == nil {
		t.Fatalf("ResolveOrCreate(b) = %v, %v", wsB, err)
	}

	if _, err := os.Stat(filepath.Join(dir, "state.db")); err != nil {
		t.Fatalf("档案库应位于工作目录：%v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "workspaces")); !os.IsNotExist(err) {
		t.Fatalf("不应再创建工作空间目录：%v", err)
	}

	wsA.State.SaveMessage(cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Content: "from-a", Time: 1})
	wsB.State.SaveMessage(cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Content: "from-b", Time: 2})
	m.Close()

	// 名单为空：只可能从档案恢复，不能再靠"目录存在"兜底。
	cfg = &config.Config{AllowGroups: []string{"g1"}, MaxCacheMsg: 10}
	m2, err := New(func() *config.Config { return cfg }, system,
		filepath.Join(dir, "prompt_custom.yaml"), nil, nil, nil, context.Background())
	if err != nil {
		t.Fatalf("重启 New() error = %v", err)
	}
	defer m2.Close()

	for id, want := range map[string]string{"qq_a": "from-a", "qq_b": "from-b"} {
		ws, ok := m2.ByID(id)
		if !ok || ws == nil {
			t.Fatalf("%s 重启后应被扫描加载", id)
		}
		msgs, err := ws.State.LoadRecent(10)
		if err != nil || len(msgs) != 1 || msgs[0].Content != want {
			t.Fatalf("%s 恢复的消息 = %+v, %v; want %q", id, msgs, err, want)
		}
	}
}

// Test群档案与成员随消息入库 EnsureGroup/ObserveMember 的写入要跨重启保留：
// 面板靠它们展示群名与“已收录成员数”。
func Test群档案与成员随消息入库(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		AllowGroups:       []string{"g1"},
		WorkspaceAccounts: []string{"qq_a"},
		MaxCacheMsg:       10,
	}
	newManager := func() *Manager {
		m, err := New(func() *config.Config { return cfg }, system,
			filepath.Join(dir, "prompt_custom.yaml"), nil, nil, nil, context.Background())
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		return m
	}

	m := newManager()
	ws, err := m.ResolveOrCreate("qq", "a")
	if err != nil || ws == nil {
		t.Fatalf("ResolveOrCreate = %v, %v", ws, err)
	}
	ws.EnsureGroup("g1")
	ws.ObserveMember("g1", "u1")
	ws.ObserveMember("g1", "u1") // 同一个人重复出现不能重复计数
	if got := ws.MemberCount("g1"); got != 1 {
		t.Fatalf("MemberCount = %d, want 1", got)
	}
	m.Close()

	m2 := newManager()
	defer m2.Close()
	if got := m2.MemberCount("qq_a", "g1"); got != 1 {
		t.Fatalf("重启后已收录成员数 = %d, want 1", got)
	}
	if got := m2.GroupName("qq_a", "g1"); got != "" {
		t.Fatalf("没有群名时应为空串，得到 %q", got)
	}
	ws2, ok := m2.ByID("qq_a")
	if !ok || ws2 == nil {
		t.Fatal("工作空间应从档案恢复")
	}
	groups, err := ws2.State.LoadGroups()
	if err != nil || groups["g1"] != "" {
		t.Fatalf("群档案应从库里恢复：%+v, %v", groups, err)
	}
}
