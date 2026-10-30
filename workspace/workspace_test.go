package workspace

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/workspace/state"
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

// TestWorkspace入站时核对昵称改名 昵称可能在平台侧被改：加载沿用 state.db 的旧名，
// 收到第一条入站消息时按 self_id / group_id 精确定位账号核对一次，变了就同时更新内存与 meta。
func TestWorkspace入站时核对昵称改名(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(filepath.Join(dir, state.FileName))
	if err != nil {
		t.Fatalf("打开 state.db：%v", err)
	}
	scope := store.Scope("qq_100")
	scope.SetMeta("platform", "qq")
	scope.SetMeta("account", "100")
	scope.SetMeta("name", "旧名")
	// 机器人自己在两个群里的成员行：g1 没设群名片（显示名等于账号名），
	// g2 设了独立名片——改名时前者要跟着改，后者不能被覆盖。
	scope.SaveIdentity("g1", "openid-100")
	scope.SaveMember("g1", "openid-100", "旧名", "旧名")
	scope.SaveIdentity("g2", "openid-100")
	scope.SaveMember("g2", "openid-100", "旧名", "专属名片")
	store.Close()

	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body struct {
			Action string         `json:"action"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Action != "get_self_info" {
			t.Errorf("action = %q", body.Action)
		}
		// 多账号共用一个实现端时，必须靠这两个参数定位是哪个机器人
		if body.Params["group_id"] != "g1" || body.Params["self_id"] != "openid-100" {
			t.Errorf("应带上定位账号的参数，实际 %+v", body.Params)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "retcode": 0,
			"data": map[string]any{"user_id": "openid-100", "user_name": "新名"},
		})
	}))
	defer server.Close()

	obClient := onebot.NewClient(server.URL, nil, "").For("qq_100")
	m, err := New(func() *config.Config { return &config.Config{} }, system,
		filepath.Join(dir, "prompt_custom.yaml"), nil, obClient, nil, context.Background())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer m.Close()

	list := m.List()
	if len(list) != 1 {
		t.Fatalf("应恢复 1 个工作空间，实际 %d", len(list))
	}
	ws := list[0]
	// 启动只读缓存，不查网络：有缓存名时保持旧名
	if got := ws.Name(); got != "旧名" {
		t.Fatalf("启动应沿用缓存昵称，实际 %q", got)
	}

	ws.SyncSelfName("openid-100", "g1")
	deadline := time.Now().Add(3 * time.Second)
	for ws.Name() != "新名" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ws.Name(); got != "新名" {
		t.Fatalf("内存昵称应更新为新名，实际 %q", got)
	}
	if got := ws.State.Meta("name"); got != "新名" {
		t.Fatalf("meta.name 应更新为新名，实际 %q", got)
	}

	// members 表也要跟着改：g1 跟随账号名，g2 的独立群名片保留
	deadline = time.Now().Add(3 * time.Second)
	for ws.DisplayNameFor("g1")("openid-100") != "新名" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ws.DisplayNameFor("g1")("openid-100"); got != "新名" {
		t.Fatalf("g1 内存成员名应更新为新名，实际 %q", got)
	}
	members, err := ws.State.LoadMembers()
	if err != nil {
		t.Fatalf("读取成员表：%v", err)
	}
	if got := members["g1"]["openid-100"]; got != "新名" {
		t.Fatalf("g1 members 行应更新为新名，实际 %q", got)
	}
	if got := members["g2"]["openid-100"]; got != "专属名片" {
		t.Fatalf("g2 独立群名片不应被覆盖，实际 %q", got)
	}

	// 节流：短时间内再来消息不会再打实现端
	ws.SyncSelfName("openid-100", "g1")
	time.Sleep(100 * time.Millisecond)
	if got := calls.Load(); got != 1 {
		t.Fatalf("同一节流窗口内只应核对一次，实际请求 %d 次", got)
	}
}

// TestWorkspace成员名先写members再镜像meta 名字的落点顺序：
// 群友/机器人的名字先落到成员目录（members），机器人自己那一行变了才镜像到 meta；
// 群友改名只留在 members，不能动工作空间展示名。
func TestWorkspace成员名先写members再镜像meta(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	store, err := state.Open(filepath.Join(dir, state.FileName))
	if err != nil {
		t.Fatalf("打开 state.db：%v", err)
	}
	scope := store.Scope("qq_100")
	scope.SetMeta("platform", "qq")
	scope.SetMeta("account", "100")
	scope.SetMeta("name", "旧名")
	scope.SaveIdentity("g1", "openid-100")
	store.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Action string         `json:"action"`
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		userID, _ := body.Params["user_id"].(string)
		name := "张三"
		if userID == "openid-100" {
			name = "群里的机器人名"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "ok", "retcode": 0,
			"data": map[string]any{"user_id": userID, "user_name": name, "user_displayname": name},
		})
	}))
	defer server.Close()

	obClient := onebot.NewClient(server.URL, nil, "").For("qq_100")
	m, err := New(func() *config.Config { return &config.Config{} }, system,
		filepath.Join(dir, "prompt_custom.yaml"), nil, obClient, nil, context.Background())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer m.Close()
	ws := m.List()[0]

	// 群友：只更新 members，工作空间展示名保持不动
	ws.DisplayNameFor("g1")("u1") // 未命中会异步查成员信息
	deadline := time.Now().Add(3 * time.Second)
	for ws.DisplayNameFor("g1")("u1") != "张三" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ws.DisplayNameFor("g1")("u1"); got != "张三" {
		t.Fatalf("群友名字应写入 members，实际 %q", got)
	}
	if got := ws.Name(); got != "旧名" {
		t.Fatalf("群友改名不应影响 meta.name，实际 %q", got)
	}

	// 机器人自己：members 先更新，随后镜像到 meta
	ws.DisplayNameFor("g1")("openid-100")
	deadline = time.Now().Add(3 * time.Second)
	for ws.Name() != "群里的机器人名" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := ws.Name(); got != "群里的机器人名" {
		t.Fatalf("机器人成员名变化应镜像到 meta.name，实际 %q", got)
	}
	if got := ws.DisplayNameFor("g1")("openid-100"); got != "群里的机器人名" {
		t.Fatalf("机器人成员名应先写入 members，实际 %q", got)
	}
	members, err := ws.State.LoadMembers()
	if err != nil {
		t.Fatalf("读取成员表：%v", err)
	}
	if got := members["g1"]["openid-100"]; got != "群里的机器人名" {
		t.Fatalf("members 行应为新名，实际 %q", got)
	}
	if got := ws.State.Meta("name"); got != "群里的机器人名" {
		t.Fatalf("meta.name 应为新名，实际 %q", got)
	}
}

// TestLearnMemberName消息自带名字 桥接透传 sender 昵称时不需要等 get_group_member_info：
// 目录立即生效，并落 members 表供重启后继续使用。
func TestLearnMemberName消息自带名字(t *testing.T) {
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
	ws.LearnMemberName("g1", "u1", "小张")
	if got := ws.DisplayNameFor("g1")("u1"); got != "小张" {
		t.Fatalf("消息自带的名字应立即生效，实际 %q", got)
	}
	// 同名重复学习是幂等的，不能把名字打回空。
	ws.LearnMemberName("g1", "u1", "小张")
	if got := ws.DisplayNameFor("g1")("u1"); got != "小张" {
		t.Fatalf("重复学习后名字不应变化，实际 %q", got)
	}
	m.Close()

	m2 := newManager()
	defer m2.Close()
	ws2, ok := m2.ByID("qq_a")
	if !ok || ws2 == nil {
		t.Fatal("工作空间应从档案恢复")
	}
	if got := ws2.DisplayNameFor("g1")("u1"); got != "小张" {
		t.Fatalf("重启后应按 members 表恢复名字，实际 %q", got)
	}
}
