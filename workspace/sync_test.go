package workspace

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"good-review-master/config"
	"good-review-master/internal/testutil"
	"good-review-master/onebot"
	"good-review-master/pipeline"
	"good-review-master/workspace/state"
)

func syncTestWorkspace(t *testing.T) (*Manager, *Workspace) {
	t.Helper()
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		AllowGroups:       []string{"g1"},
		WorkspaceAccounts: []string{"qq_official_bot_1906"},
		MaxCacheMsg:       100,
		MaxMsgRune:        500,
		LLMSendCount:      10,
	}
	mgr, err := New(func() *config.Config { return cfg }, system,
		filepath.Join(dir, "prompt_custom.yaml"), testutil.NewFakeLLM(), nil, nil, context.Background())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(mgr.Close)
	ws, err := mgr.ResolveOrCreate("qq", AccountKindOfficialBot, "1906")
	if err != nil || ws == nil {
		t.Fatalf("ResolveOrCreate: %v", err)
	}
	return mgr, ws
}

func syncEvent(messageID, userID, text string, mentions ...string) onebot.Event {
	segments := []onebot.Segment{onebot.TextSegment(text)}
	for _, id := range mentions {
		segments = append(segments, onebot.MentionSegment(id))
	}
	return onebot.Event{
		Type: "message", DetailType: "group", GroupID: "g1",
		UserID: userID, MessageID: messageID,
		Self:    onebot.Self{Platform: "qq", UserID: "bot-openid"},
		Message: segments,
		Time:    100,
	}
}

func TestPrepareBatch批量补名并同步入库(t *testing.T) {
	mgr, ws := syncTestWorkspace(t)
	// 先给 u2 建好名字：批量补名应该从 members 表一次取回。
	scope := mgr.StateStore().Scope(ws.ID)
	if _, err := scope.SaveMemberInfo(state.MemberInfo{
		GroupID: "g1", UserID: "u2", Name: "李四", DisplayName: "李四", Source: "member_info",
	}); err != nil {
		t.Fatal(err)
	}

	events := []pipeline.InboundEvent{
		{WorkspaceID: ws.ID, Event: syncEvent("m1", "u1", "大家好")},
		{WorkspaceID: ws.ID, Event: syncEvent("m2", "u2", "我是李四")},
		{WorkspaceID: ws.ID, Event: syncEvent("m3", "u3", "找个人", "u2")},
	}
	synced := ws.PrepareBatch(context.Background(), events)
	if len(synced) != 3 {
		t.Fatalf("应产出 3 条，实际 %d", len(synced))
	}
	if synced[1].Message.Nickname != "李四" {
		t.Fatalf("u2 昵称 = %q", synced[1].Message.Nickname)
	}
	if !strings.Contains(synced[2].Message.Content, "@李四(u2)") {
		t.Fatalf("mention 应渲染成 @昵称(ID)：%q", synced[2].Message.Content)
	}

	// 同步入库：PrepareBatch 返回后立刻能读到，不需要等待任何队列。
	msgs, err := ws.State.LoadRecent(10)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("LoadRecent = %+v, %v", msgs, err)
	}
}

func TestPrepareBatch过滤重复与白名单外群(t *testing.T) {
	_, ws := syncTestWorkspace(t)
	event := syncEvent("m1", "u1", "你好")
	synced := ws.PrepareBatch(context.Background(), []pipeline.InboundEvent{
		{WorkspaceID: ws.ID, Event: event},
		{WorkspaceID: ws.ID, Event: event},
	})
	if len(synced) != 1 {
		t.Fatalf("同一批重复消息应只保留一条，实际 %d", len(synced))
	}

	other := syncEvent("m2", "u1", "别的群")
	other.GroupID = "g2"
	if got := ws.PrepareBatch(context.Background(), []pipeline.InboundEvent{{WorkspaceID: ws.ID, Event: other}}); len(got) != 0 {
		t.Fatalf("白名单外的群应被过滤，实际 %+v", got)
	}
}
