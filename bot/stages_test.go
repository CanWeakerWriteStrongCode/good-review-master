package bot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"good-review-master/config"
	"good-review-master/conversation"
	"good-review-master/internal/testutil"
	"good-review-master/pipeline"
	"good-review-master/workspace"
)

// TestDeadLetter报警带上账号 验证阶段重试超限写报警时账号没丢。
//
// Item 里只有 stage / key / id，没有账号字段，账号是从分区键
// 「workspaceID|chatID」拆出来的；拆不出来面板上就只能显示「全局」。
func TestDeadLetter报警带上账号(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{WorkspaceAccounts: []string{"qq_official_bot_1906"}}
	snapshot := func() *config.Config { return cfg }
	mgr, err := workspace.New(snapshot, system, filepath.Join(dir, "prompt_custom.yaml"),
		testutil.NewFakeLLM(), nil, nil, context.Background())
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	t.Cleanup(mgr.Close)

	// 真实链路里的 key 就是这个拼法（bot/ingest.go、bot/stages.go）。
	key := conversation.ChatKey("qq_official_bot_1906", "g1")
	item, err := pipeline.NewItem(pipeline.StageSync, key, "m1",
		conversation.Inbound{WorkspaceID: "qq_official_bot_1906"})
	if err != nil {
		t.Fatalf("NewItem: %v", err)
	}

	b := NewBot(snapshot, mgr)
	b.deadLetter(item, errors.New("boom"))

	list, _, err := mgr.StateStore().LoadAlerts(50)
	if err != nil {
		t.Fatalf("LoadAlerts: %v", err)
	}
	for _, alert := range list {
		if alert.Kind != "dead_letter_sync" {
			continue
		}
		if alert.WorkspaceID != "qq_official_bot_1906" {
			t.Fatalf("报警账号 = %q，want qq_official_bot_1906", alert.WorkspaceID)
		}
		if !strings.Contains(alert.Detail, `"chat":"g1"`) {
			t.Fatalf("现场里没有群号：%s", alert.Detail)
		}
		return
	}
	t.Fatal("没找到 dead_letter_sync 报警")
}
