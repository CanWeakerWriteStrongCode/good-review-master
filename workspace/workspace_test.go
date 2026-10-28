package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

func testManager(t *testing.T, cfg *config.Config) *Manager {
	t.Helper()
	logutil.SetupLogger()
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0644); err != nil {
		t.Fatal(err)
	}
	manager, err := New(func() *config.Config { return cfg }, system,
		filepath.Join(dir, "prompt_custom.yaml"), nil, nil, nil, context.Background())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return manager
}

func twoWorkspaceConfig() *config.Config {
	return &config.Config{
		AllowGroups:  []string{"g1"},
		AllowSelfIDs: []string{"*"},
		Workspaces: []config.WorkspaceConf{
			{ID: "a", Name: "账号A", PlatformID: "平台A", SelfIDs: []string{"a1"}, AllowGroups: []string{"g1"}},
			{ID: "b", Name: "账号B", PlatformID: "平台B", SelfIDs: []string{"b1"}, AllowGroups: []string{"g1", "g2"}},
		},
	}
}

func TestResolve以platformID优先(t *testing.T) {
	m := testManager(t, twoWorkspaceConfig())

	cases := []struct {
		name  string
		event onebot.Event
		want  string
	}{
		{"平台实例命中，忽略冲突的 self_id", onebot.Event{PlatformID: "平台A", SelfID: "b1"}, "a"},
		{"没有 platform_id 时按 self_id 别名", onebot.Event{SelfID: "b1"}, "b"},
		{"占位 self_id 且无 platform_id 时不猜", onebot.Event{SelfID: "qq_official"}, ""},
		{"两者都不认识时不接管", onebot.Event{PlatformID: "平台X", SelfID: "x1"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := m.Resolve(tc.event)
			if tc.want == "" {
				if got != nil {
					t.Fatalf("Resolve() = %s，期望 nil", got.ID)
				}
				return
			}
			if got == nil || got.ID != tc.want {
				t.Fatalf("Resolve() = %+v，期望 %s", got, tc.want)
			}
		})
	}
}

func TestResolve单工作空间兜底(t *testing.T) {
	cfg := &config.Config{
		AllowGroups: []string{"g1"},
		Workspaces: []config.WorkspaceConf{
			{ID: "only", PlatformID: "平台A", SelfIDs: []string{"a1"}},
		},
	}
	m := testManager(t, cfg)
	if ws := m.Resolve(onebot.Event{SelfID: "unknown_selfid"}); ws == nil || ws.ID != "only" {
		t.Fatalf("单工作空间应兜底，得到 %+v", ws)
	}
}

func Test工作空间缓存隔离(t *testing.T) {
	m := testManager(t, twoWorkspaceConfig())
	wsA := m.Resolve(onebot.Event{PlatformID: "平台A"})
	wsB := m.Resolve(onebot.Event{PlatformID: "平台B"})

	wsA.Cache.GetGroupCache("g1", 8).Add(cache.Message{MessageID: 1, SelfID: "a1", Content: "a", Time: 1})
	wsB.Cache.GetGroupCache("g1", 8).Add(cache.Message{MessageID: 1, SelfID: "b1", Content: "b", Time: 1})

	if got := wsA.Cache.GetGroupCache("g1", 8).Len(); got != 1 {
		t.Fatalf("账号 A 缓存 = %d, want 1", got)
	}
	if got := wsB.Cache.GetGroupCache("g1", 8).Len(); got != 1 {
		t.Fatalf("账号 B 缓存 = %d, want 1", got)
	}
	if wsA.Cache.GetGroupCache("g1", 8).GetAll()[0].Content != "a" {
		t.Fatal("账号 A 的缓存被账号 B 污染")
	}
}

func Test工作空间提示词隔离(t *testing.T) {
	m := testManager(t, twoWorkspaceConfig())
	wsA, _ := m.ByID("a")
	wsB, _ := m.ByID("b")

	if err := wsA.Prompt.AddCommand("chat_review", "只在A", "提示词", nil); err != nil {
		t.Fatalf("AddCommand() error = %v", err)
	}
	wsA.Prompt.Reload()

	if len(wsA.Prompt.Snapshot().CmdConfigs["chat_review"]) != 1 {
		t.Fatal("账号 A 应看到自己添加的指令")
	}
	if len(wsB.Prompt.Snapshot().CmdConfigs["chat_review"]) != 0 {
		t.Fatal("账号 B 不应看到账号 A 添加的指令")
	}
}

func Test工作空间群白名单继承与覆盖(t *testing.T) {
	m := testManager(t, twoWorkspaceConfig())
	wsA, _ := m.ByID("a")
	wsB, _ := m.ByID("b")

	if wsA.AllowsGroup("g2") {
		t.Fatal("账号 A 只声明了 g1，不应放行 g2")
	}
	if !wsB.AllowsGroup("g2") {
		t.Fatal("账号 B 声明了 g2，应放行")
	}
}

func TestManager识别昵称唯一性(t *testing.T) {
	cfg := twoWorkspaceConfig()
	cfg.Workspaces[0].Nickname = "同名"
	cfg.Workspaces[1].Nickname = "同名"
	m := testManager(t, cfg)
	if m.NicknameUnique("同名") {
		t.Fatal("两个账号同名时不能用昵称做归属兜底")
	}

	cfg.Workspaces[1].Nickname = "另一个"
	m = testManager(t, cfg)
	if !m.NicknameUnique("另一个") {
		t.Fatal("昵称唯一时应允许兜底")
	}
}

func TestBindEvent补齐占位身份(t *testing.T) {
	m := testManager(t, twoWorkspaceConfig())
	ws, _ := m.ByID("a")

	event := ws.BindEvent(onebot.Event{SelfID: "qq_official"})
	if event.SelfID != "a1" || event.PlatformID != "平台A" {
		t.Fatalf("占位 self_id 应回填为工作空间身份：%+v", event)
	}

	// 真实 self_id 不覆盖：同账号可能有多个别名，事件里那个才是本次接收者。
	event = ws.BindEvent(onebot.Event{SelfID: "a2"})
	if event.SelfID != "a2" {
		t.Fatalf("真实 self_id 不该被覆盖：%+v", event)
	}
}

func TestIsManagedAccount跨工作空间(t *testing.T) {
	m := testManager(t, twoWorkspaceConfig())
	if !m.IsManagedAccount("a1") || !m.IsManagedAccount("b1") {
		t.Fatal("两个账号的消息都应被识别为托管账号")
	}
	if m.IsManagedAccount("u1") {
		t.Fatal("普通群友不应被当成托管账号")
	}
}
