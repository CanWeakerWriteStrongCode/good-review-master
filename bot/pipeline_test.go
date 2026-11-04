package bot

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"good-review-master/config"
	"good-review-master/conversation"
	"good-review-master/internal/testutil"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/workspace"
)

// TestMain 把 cwd 切到临时目录并初始化 logger：workspace.New 启动时会打日志。
func TestMain(m *testing.M) {
	original, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	temp, err := os.MkdirTemp("", "bot-test-")
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

// Test消息走完三段流水线 验证 M0 的 IM→B1→Stage1→B2→Stage2→B3→Stage3 主链路。
func Test消息走完三段流水线(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		AllowGroups:       []string{"g1"},
		WorkspaceAccounts: []string{"qq_official_bot_1906"},
		BotNickname:       "小助理",
		MaxCacheMsg:       100,
		MaxMsgRune:        500,
		LLMSendCount:      10,
		LLMTimeout:        2 * time.Second,
		LLMConfig:         config.LLMConf{},
	}
	snapshot := func() *config.Config { return cfg }
	fake := testutil.NewFakeLLM()
	mgr, err := workspace.New(snapshot, system, filepath.Join(dir, "prompt_custom.yaml"),
		fake, nil, nil, context.Background())
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	t.Cleanup(mgr.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := NewBot(snapshot, mgr)
	go b.Run(ctx)

	event := onebot.Event{
		ID:          "e1",
		Time:        float64(time.Now().Unix()),
		Type:        "message",
		DetailType:  "group",
		Self:        onebot.Self{Platform: "qq", UserID: "bot-openid"},
		AccountKind: "official_bot",
		MessageID:   "m1",
		GroupID:     "g1",
		UserID:      "u1",
		Message: []onebot.Segment{
			onebot.TextSegment("你好"),
			onebot.MentionSegment("bot-openid"),
		},
		AltMessage: "@小助理 你好",
	}
	accepted, err := b.AcceptEvent(ctx, "1906", event)
	if err != nil || !accepted {
		t.Fatalf("AcceptEvent = %v, %v", accepted, err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for len(fake.Calls()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	calls := fake.Calls()
	if len(calls) == 0 {
		t.Fatal("消息没有走到 Stage 3（FakeLLM 未被调用）")
	}
	if !strings.Contains(calls[0].ChatLog, "你好") {
		t.Fatalf("发给模型的群聊记录缺少正文：%s", calls[0].ChatLog)
	}

	ws, ok := mgr.ByID("qq_official_bot_1906")
	if !ok {
		t.Fatal("工作空间未开通")
	}
	msgs := ws.Cache.GetGroupCache("g1", 100).GetAll()
	found := false
	for _, msg := range msgs {
		if msg.MessageID == "m1" && strings.Contains(msg.Content, "你好") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("入站消息没有进入窗口缓存：%+v", msgs)
	}
}

func Test图片消息在Stage2生成描述并入库(t *testing.T) {
	dir := t.TempDir()
	imgPath := filepath.Join(dir, "a.png")
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(imgPath, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		AllowGroups:       []string{"g1"},
		WorkspaceAccounts: []string{"qq_official_bot_1906"},
		BotNickname:       "小助理",
		MaxCacheMsg:       100,
		MaxMsgRune:        500,
		LLMSendCount:      10,
		LLMTimeout:        2 * time.Second,
		Assets: config.AssetsConf{
			MessageBudget: 5 * time.Second, TagConcurrency: 1, PromptVersion: "test",
		},
	}
	cfg.LLMConfig.ImageNoteMaxSide = 640
	cfg.LLMConfig.ImageNoteMaxPixels = 400000
	cfg.LLMConfig.ImageNoteTimeoutSec = 2
	cfg.LLMConfig.ImageNoteRetries = 2
	cfg.LLMConfig.ImageLocalDirs = []string{dir}
	snapshot := func() *config.Config { return cfg }
	fake := testutil.NewFakeLLM()
	fake.SetStructuredReply(`{"content":"一张测试图","info_level":"low","has_text":false}`)
	mgr, err := workspace.New(snapshot, system, filepath.Join(dir, "prompt_custom.yaml"),
		fake, nil, nil, context.Background())
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	t.Cleanup(mgr.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := NewBot(snapshot, mgr)
	go b.Run(ctx)

	event := onebot.Event{
		ID: "e1", Time: float64(time.Now().Unix()), Type: "message", DetailType: "group",
		Self: onebot.Self{Platform: "qq", UserID: "bot-openid"}, AccountKind: "official_bot",
		MessageID: "m1", GroupID: "g1", UserID: "u1",
		Message: []onebot.Segment{
			onebot.TextSegment("看看这个"),
			onebot.ImageSegment(imgPath),
			onebot.MentionSegment("bot-openid"),
		},
	}
	accepted, err := b.AcceptEvent(ctx, "1906", event)
	if err != nil || !accepted {
		t.Fatalf("AcceptEvent = %v, %v", accepted, err)
	}

	ws, ok := mgr.ByID("qq_official_bot_1906")
	if !ok {
		t.Fatal("工作空间未开通")
	}
	deadline := time.Now().Add(3 * time.Second)
	var msgs []conversation.Message
	for time.Now().Before(deadline) {
		msgs = ws.Cache.GetGroupCache("g1", 100).GetAll()
		if len(msgs) == 1 && len(msgs[0].ImageNotes) == 1 && msgs[0].ImageNotes[0].Note != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(msgs) != 1 || len(msgs[0].ImageNotes) != 1 || msgs[0].ImageNotes[0].Note != "一张测试图" {
		t.Fatalf("Stage 2 描述未进缓存：%+v", msgs)
	}
	if len(msgs[0].ImageIDs) != 1 || msgs[0].ImageIDs[0].SHA == "" {
		t.Fatalf("图片标识缺失：%+v", msgs[0].ImageIDs)
	}

	persisted, err := ws.State.LoadRecent(10)
	if err != nil || len(persisted) != 1 || len(persisted[0].ImageIDs) != 1 {
		t.Fatalf("图片标识未入库：%+v, %v", persisted, err)
	}
	if len(persisted[0].ImageNotes) != 1 || persisted[0].ImageNotes[0].Note != "一张测试图" {
		t.Fatalf("按 id 回填描述失败：%+v", persisted[0].ImageNotes)
	}
}

// Test只记录账号不回复但照常入缓存 验证 record_only_accounts：
// 消息照常走到 Stage 3 进缓存，但大模型一次都不会被调用。
func Test只记录账号不回复但照常入缓存(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		AllowGroups:        []string{"g1"},
		WorkspaceAccounts:  []string{"qq_official_bot_1906"},
		RecordOnlyAccounts: []string{"qq_official_bot_1906"},
		BotNickname:        "小助理",
		MaxCacheMsg:        100,
		MaxMsgRune:         500,
		LLMSendCount:       10,
		LLMTimeout:         2 * time.Second,
	}
	snapshot := func() *config.Config { return cfg }
	fake := testutil.NewFakeLLM()
	mgr, err := workspace.New(snapshot, system, filepath.Join(dir, "prompt_custom.yaml"),
		fake, nil, nil, context.Background())
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	t.Cleanup(mgr.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := NewBot(snapshot, mgr)
	go b.Run(ctx)

	event := onebot.Event{
		ID:          "e1",
		Time:        float64(time.Now().Unix()),
		Type:        "message",
		DetailType:  "group",
		Self:        onebot.Self{Platform: "qq", UserID: "bot-openid"},
		AccountKind: "official_bot",
		MessageID:   "m1",
		GroupID:     "g1",
		UserID:      "u1",
		Message: []onebot.Segment{
			onebot.TextSegment("你好"),
			onebot.MentionSegment("bot-openid"),
		},
		AltMessage: "@小助理 你好",
	}
	if accepted, err := b.AcceptEvent(ctx, "1906", event); err != nil || !accepted {
		t.Fatalf("AcceptEvent = %v, %v", accepted, err)
	}

	ws, ok := mgr.ByID("qq_official_bot_1906")
	if !ok {
		t.Fatal("工作空间未开通")
	}
	// 缓存能出现，说明 Stage 3 已经处理过这条消息了；再等一小会儿确认它没继续往下调模型。
	deadline := time.Now().Add(3 * time.Second)
	var msgs []conversation.Message
	for time.Now().Before(deadline) {
		msgs = ws.Cache.GetGroupCache("g1", 100).GetAll()
		if len(msgs) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(msgs) != 1 || msgs[0].MessageID != "m1" {
		t.Fatalf("只记录账号的消息没有进缓存：%+v", msgs)
	}
	time.Sleep(200 * time.Millisecond)
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("只记录账号不应调用大模型，实际 %d 次", len(calls))
	}
}

// Test管理指令走独立阶段 内部指令不该进 Stage 2/3：不调大模型、不进群聊缓存。
// 走旧路径时 #帮助 会先被 gc.Add 再回复，所以“缓存里没有它”就是分流生效的证据。
func Test管理指令走独立阶段(t *testing.T) {
	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte("cmd: {}\nrules: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		AllowGroups:       []string{"g1"},
		WorkspaceAccounts: []string{"qq_official_bot_1906"},
		BotNickname:       "小助理",
		CommandAdmins:     map[string][]string{"g1": {"u1"}},
		MaxCacheMsg:       100,
		MaxMsgRune:        500,
		LLMSendCount:      10,
		LLMTimeout:        2 * time.Second,
	}
	snapshot := func() *config.Config { return cfg }
	fake := testutil.NewFakeLLM()
	mgr, err := workspace.New(snapshot, system, filepath.Join(dir, "prompt_custom.yaml"),
		fake, nil, nil, context.Background())
	if err != nil {
		t.Fatalf("workspace.New: %v", err)
	}
	t.Cleanup(mgr.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b := NewBot(snapshot, mgr)
	go b.Run(ctx)

	event := onebot.Event{
		ID:          "e1",
		Time:        float64(time.Now().Unix()),
		Type:        "message",
		DetailType:  "group",
		Self:        onebot.Self{Platform: "qq", UserID: "bot-openid"},
		AccountKind: "official_bot",
		MessageID:   "m1",
		GroupID:     "g1",
		UserID:      "u1",
		Message: []onebot.Segment{
			onebot.TextSegment("#帮助"),
			onebot.MentionSegment("bot-openid"),
		},
		AltMessage: "@小助理 #帮助",
	}
	if accepted, err := b.AcceptEvent(ctx, "1906", event); err != nil || !accepted {
		t.Fatalf("AcceptEvent = %v, %v", accepted, err)
	}

	// 给 Stage 1/Command 足够时间跑完；若仍走旧路径，这会儿已经进缓存了。
	time.Sleep(500 * time.Millisecond)
	ws, ok := mgr.ByID("qq_official_bot_1906")
	if !ok {
		t.Fatal("工作空间未开通")
	}
	// 机器人自己的回复会进缓存（recordSelfReply），但入站的那条指令本体不该进：
	// 它没走 Stage 3，也就没有 gc.Add。
	msgs := ws.Cache.GetGroupCache("g1", 100).GetAll()
	replied := false
	for _, msg := range msgs {
		if msg.MessageID == "m1" {
			t.Fatalf("管理指令本体不应进入群聊缓存：%+v", msg)
		}
		if strings.Contains(msg.Content, "指令帮助") {
			replied = true
		}
	}
	if !replied {
		t.Fatalf("指令没有得到回复，可能根本没执行：%+v", msgs)
	}
	if calls := fake.Calls(); len(calls) != 0 {
		t.Fatalf("管理指令不应调用大模型，实际 %d 次", len(calls))
	}

	// 拓扑要覆盖全部回执：前置回的是工作流名（chat / command），
	// 域回的是去向（tagged / command_done / replied）。后两个没有下一站，
	// 必须是显式终点——不能靠"没连线"推断，那样漏一条线就会变成静默少走一步。
	assertTopicsCovered(t, b.orch,
		WorkflowChat, WorkflowCommand, TopicTagged, TopicCommandDone, TopicReplied)
	if b.orch.router == nil {
		t.Fatal("应当挂了一个前置")
	}
	if len(b.orch.workflows) != 2 {
		t.Fatalf("应当有两条工作流，实际 %d 条：%v", len(b.orch.workflows), b.orch.workflows)
	}
	if !b.orch.exits[TopicCommandDone] {
		t.Fatal("command_done 应当被声明为终点")
	}
	if !b.orch.exits[TopicReplied] {
		t.Fatal("replied 应当被声明为终点")
	}
}
