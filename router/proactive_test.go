package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/internal/testutil"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

// ---- 纯函数：解析与窗口 ----

func TestParseJudgeVerdict(t *testing.T) {
	yes := true
	no := false

	cases := []struct {
		name    string
		raw     string
		want    *bool
		wantErr bool
	}{
		{name: "干净JSON", raw: `{"reply": true, "reason": "值得接话"}`, want: &yes},
		{name: "代码围栏", raw: "```json\n{\"reply\": false, \"reason\": \"私聊\"}\n```", want: &no},
		{name: "前后有散文", raw: `我觉得应该回：{"reply": true, "reason": "有人提问"} 完毕`, want: &yes},
		{name: "缺reply字段", raw: `{"reason": "没有结论"}`, wantErr: true},
		{name: "非JSON", raw: "不回", wantErr: true},
		{name: "空输出", raw: "", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseJudgeVerdict(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望解析失败，实际 %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败：%v", err)
			}
			if got.Reply == nil || *got.Reply != *tc.want {
				t.Fatalf("reply = %v，期望 %v", got.Reply, *tc.want)
			}
		})
	}
}

func TestJudgeWindow以候选消息为右边界(t *testing.T) {
	msgs := []cache.Message{
		{MessageID: "1", Content: "a"},
		{MessageID: "2", Content: "b"},
		{MessageID: "3", Content: "c"},
		{MessageID: "4", Content: "d"},
	}

	got := judgeWindow(msgs, "2", 10)
	if len(got) != 2 || got[1].MessageID != "2" {
		t.Fatalf("应以候选消息为右边界，实际 %+v", got)
	}

	got = judgeWindow(msgs, "2", 1)
	if len(got) != 1 || got[0].MessageID != "2" {
		t.Fatalf("limit 应截断到候选消息本身，实际 %+v", got)
	}

	// 候选消息已不在缓存（被环形缓冲覆盖）：退回最近 limit 条
	got = judgeWindow(msgs, "unknown", 2)
	if len(got) != 2 || got[1].MessageID != "4" {
		t.Fatalf("候选缺失时应取最近 limit 条，实际 %+v", got)
	}
}

func TestIsPlaceholderOnly(t *testing.T) {
	cases := map[string]bool{
		"[图片]":        true,
		" [图片] [图片] ": true,
		"看这个 [图片]":    false,
		"今天天气不错":      false,
	}
	for content, want := range cases {
		if got := isPlaceholderOnly(content); got != want {
			t.Fatalf("isPlaceholderOnly(%q) = %v，期望 %v", content, got, want)
		}
	}
}

// ---- 状态机：门控与单槽合并 ----

func TestProactiveRegistry门控与合并(t *testing.T) {
	now := time.Now()
	cfg := &config.Config{
		ProactiveCooldown:     90 * time.Second,
		ProactiveMaxStreak:    2,
		ProactiveJudgeEvery:   20 * time.Second,
		ProactiveJudgeTimeout: 20 * time.Second,
	}
	var reg proactiveRegistry

	first := &proactiveCandidate{content: "第一条"}
	if !reg.enqueue("g1", first, cfg, now) {
		t.Fatal("空闲时应起 worker")
	}

	// 运行期间新消息只覆盖单槽，返回 false（不再起 worker）
	second := &proactiveCandidate{content: "第二条"}
	if reg.enqueue("g1", second, cfg, now) {
		t.Fatal("已有 worker 时不应再起一个")
	}

	// worker 出队：直接拿到被覆盖后的最新一条
	cand, done := reg.next("g1", cfg, now)
	if done || cand == nil || cand.content != "第二条" {
		t.Fatalf("应取到合并后的最新候选，got=%+v done=%v", cand, done)
	}
	if _, done := reg.next("g1", cfg, now); !done {
		t.Fatal("没有 pending 时应让 worker 退出")
	}

	// 判定间隔：刚判定过，20s 内不再接受新候选
	if reg.enqueue("g1", first, cfg, now.Add(5*time.Second)) {
		t.Fatal("判定间隔内不应再起判定")
	}
	// 超过间隔后恢复
	if !reg.enqueue("g1", first, cfg, now.Add(21*time.Second)) {
		t.Fatal("超过判定间隔后应恢复判定")
	}
	// 出队会把 running 复位：第一次取到候选，第二次看到空槽后退出
	if _, done := reg.next("g1", cfg, now.Add(21*time.Second)); done {
		t.Fatal("worker 不应提前退出")
	}
	if _, done := reg.next("g1", cfg, now.Add(21*time.Second)); !done {
		t.Fatal("没有 pending 时 worker 应退出")
	}

	// 发言冷却：刚主动发言后不再判定（此处已越过 20s 判定间隔）
	reg.recordReply("g1", now.Add(22*time.Second))
	if reg.enqueue("g1", first, cfg, now.Add(60*time.Second)) {
		t.Fatal("冷却期内不应再起判定")
	}
	// 冷却结束但连续发言已到上限（2 次）
	reg.recordReply("g1", now.Add(200*time.Second))
	if reg.enqueue("g1", first, cfg, now.Add(250*time.Second)) {
		t.Fatal("到达连续发言上限后不应再主动发言")
	}

	// 群静默超过 5 分钟视为新话题，计数清零
	if !reg.enqueue("g1", first, cfg, now.Add(10*time.Minute)) {
		t.Fatal("静默超时后应清零连续计数")
	}
}

func TestProactiveRegistry被点名清零(t *testing.T) {
	cfg := &config.Config{ProactiveCooldown: time.Second, ProactiveMaxStreak: 1, ProactiveJudgeEvery: time.Second}
	now := time.Now()
	var reg proactiveRegistry
	reg.recordReply("g1", now)
	if reg.enqueue("g1", &proactiveCandidate{}, cfg, now.Add(2*time.Second)) {
		t.Fatal("连续上限为 1 时不应再发言")
	}
	// 被 @ 回复后计数清零，主动链路恢复（仍受冷却约束，这里跨过冷却）
	reg.noteDirect("g1", now.Add(2*time.Second))
	if !reg.enqueue("g1", &proactiveCandidate{}, cfg, now.Add(5*time.Second)) {
		t.Fatal("被点名后连续计数应清零")
	}
}

// ---- 全链路 ----

func newProactiveTestRouter(t *testing.T, cfg *config.Config) (*Router, *testutil.FakeLLM) {
	t.Helper()
	t.Chdir(t.TempDir())
	logutil.SetupLogger()
	t.Cleanup(logutil.Close)
	cache.ResetAll()

	systemPath := filepath.Join(t.TempDir(), "prompt_system.yaml")
	if err := os.WriteFile(systemPath, []byte(rebuildTestSystemYAML), 0o600); err != nil {
		t.Fatalf("写系统提示词失败：%v", err)
	}
	promptCfg, err := config.LoadPromptConfig(systemPath, config.CustomPromptPath(systemPath))
	if err != nil {
		t.Fatalf("加载提示词失败：%v", err)
	}

	fake := testutil.NewFakeLLM()
	// 出站用一个永远成功的假实现端：否则“发送失败”会让回复不进入历史，
	// 与这些用例要验证的“生成/排版”行为无关。
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "retcode": 0, "data": map[string]any{}})
	}))
	t.Cleanup(server.Close)
	instance := NewRouter(staticSnapshot(cfg), promptCfg, fake, onebot.NewClient(server.URL, nil, "").For("test-ws"), nil, context.Background())
	instance.SetWorkspace("test-ws")
	return instance, fake
}

func proactiveTestConfig() *config.Config {
	return &config.Config{
		BotNickname:           "bot",
		MaxCacheMsg:           100,
		LLMSendCount:          20,
		LLMTimeout:            2 * time.Second,
		ProactiveEnabled:      true,
		ProactiveCooldown:     90 * time.Second,
		ProactiveMaxStreak:    5,
		ProactiveJudgeEvery:   20 * time.Second,
		ProactiveJudgeTimeout: 2 * time.Second,
		ProactiveJudgeImages:  1,
		LLMConfig: config.LLMConf{
			CacheHitCost:         0.033,
			CacheMissCost:        1.0,
			ReplyRewriteMinRunes: 60,
			MaxContextTokens:     50000,
		},
	}
}

func proactiveEvent(userID, messageID, text string, extra ...onebot.Segment) onebot.Event {
	segments := append([]onebot.Segment{onebot.TextSegment(text)}, extra...)
	return onebot.Event{
		Type:       "message",
		DetailType: "group",
		GroupID:    "g1",
		UserID:     userID,
		MessageID:  messageID,
		Self:       onebot.Self{Platform: "qq", UserID: "bot-openid"},
		Message:    segments,
	}
}

func seedMessages(r *Router, cfg *config.Config, msgs ...cache.Message) {
	gc := r.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg)
	for _, msg := range msgs {
		gc.Add(msg)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// Test裁判窗口与生成一致 裁判看到的条数取自 runtime.llm_send_count，
// 不再单独准备一份聊天记录；生成阶段自己再按缓存命中扩展/重置。
func Test裁判窗口与生成一致(t *testing.T) {
	cfg := proactiveTestConfig()
	cfg.LLMSendCount = 3
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetJSONReply(`{"reply": false, "long_form": false, "focus": "最后一条消息", "reason": "闲聊"}`)

	contents := []string{"第1条消息", "第2条消息", "第3条消息", "第4条消息", "第5条消息", "第6条消息"}
	for i, content := range contents {
		seedMessages(instance, cfg, cache.Message{
			MessageID: fmt.Sprintf("m%d", i+1), GroupID: "g1", UserID: "u1",
			Nickname: "张三", Content: content,
		})
	}
	instance.TryProactive(context.Background(), proactiveEvent("u1", "m6", contents[5]), "g1", contents[5])

	waitFor(t, "裁判调用", func() bool { return len(fake.JSONCalls()) == 1 })
	text := fake.JSONCalls()[0].Messages[1].Content
	for i := 0; i < 3; i++ {
		if strings.Contains(text, contents[i]) {
			t.Fatalf("裁判不应看到 %s（窗口=%d）：%s", contents[i], cfg.LLMSendCount, text)
		}
	}
	for i := 3; i < len(contents); i++ {
		if !strings.Contains(text, contents[i]) {
			t.Fatalf("裁判应看到 %s：%s", contents[i], text)
		}
	}
}

func TestProactive判定为是要生成回复(t *testing.T) {
	cfg := proactiveTestConfig()
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetJSONReply(`{"reply": true, "reason": "有群友提问"}`)
	fake.SetReply("可以这样解决：……")

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "有人知道这个报错怎么解吗"},
		cache.Message{MessageID: "m2", GroupID: "g1", UserID: "u2", Nickname: "李四", Content: "同问"},
	)
	event := proactiveEvent("u2", "m2", "同问")
	instance.TryProactive(context.Background(), event, "g1", "同问")

	waitFor(t, "裁判调用", func() bool { return len(fake.JSONCalls()) == 1 })
	waitFor(t, "生成调用", func() bool { return len(fake.Calls()) == 1 })
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}

	// 裁判收到了窗口与待判定提示，且 system prompt 是固定的裁判提示词
	judge := fake.JSONCalls()[0]
	if judge.Schema == nil {
		t.Fatal("裁判调用应携带 JSON schema")
	}
	if !strings.Contains(judge.Messages[0].Content, "发言决策器") {
		t.Fatalf("裁判 system prompt 不对：%s", judge.Messages[0].Content)
	}
	if !strings.Contains(judge.Messages[1].Content, "有人知道这个报错怎么解吗") {
		t.Fatalf("裁判窗口缺少聊天记录：%s", judge.Messages[1].Content)
	}
	// 裁判要能拿机器人 ID 和聊天记录里的 @昵称(ID) 对比，才知道 @ 的是不是自己
	if !strings.Contains(judge.Messages[1].Content, "ID：bot-openid") {
		t.Fatalf("裁判缺少机器人 ID，无法分辨 @ 的是自己还是别人：%s", judge.Messages[1].Content)
	}

	// 生成用的是"主动参与"提示词，而不是"根据最后@你的群友"
	gen := fake.Calls()[0]
	if !strings.Contains(gen.SystemPrompt, "主动") {
		t.Fatalf("生成提示词缺少主动参与说明：%s", gen.SystemPrompt)
	}
	if strings.Contains(gen.SystemPrompt, "根据最后@你的群友") {
		t.Fatalf("主动插话不应沿用 @ 版要求：%s", gen.SystemPrompt)
	}

	// 冷却生效：紧接着再来一条不会被判定
	instance.TryProactive(context.Background(), proactiveEvent("u1", "m3", "还有人吗"), "g1", "还有人吗")
	time.Sleep(100 * time.Millisecond)
	if got := len(fake.JSONCalls()); got != 1 {
		t.Fatalf("主动发言后应进入冷却，裁判调用 = %d", got)
	}
}

func TestProactive判定为否不生成(t *testing.T) {
	cfg := proactiveTestConfig()
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetJSONReply(`{"reply": false, "reason": "两人私聊"}`)

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "晚上吃啥"},
	)
	instance.TryProactive(context.Background(), proactiveEvent("u1", "m1", "晚上吃啥"), "g1", "晚上吃啥")

	waitFor(t, "裁判调用", func() bool { return len(fake.JSONCalls()) == 1 })
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}
	if got := len(fake.Calls()); got != 0 {
		t.Fatalf("裁判说不回时不应生成回复，实际调用 %d 次", got)
	}
}

func TestProactive门控跳过定向消息(t *testing.T) {
	cfg := proactiveTestConfig()
	instance, fake := newProactiveTestRouter(t, cfg)
	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "在吗"},
	)

	cases := map[string]onebot.Event{
		"@全体":  proactiveEvent("u1", "m1", "在吗", onebot.Segment{Type: onebot.SegmentMentionAll}),
		"@别人":  proactiveEvent("u1", "m1", "在吗", onebot.MentionSegment("u2")),
		"引用别人": proactiveEvent("u1", "m1", "在吗", onebot.ReplySegment("m0")),
	}
	for name, event := range cases {
		instance.TryProactive(context.Background(), event, "g1", "在吗")
		if got := len(fake.JSONCalls()); got != 0 {
			t.Fatalf("%s 不应进入裁判，实际调用 %d 次", name, got)
		}
	}
}

// TestProactive点名消息直接回答 点名（@ 本账号）必须回答：
// 即使调用方把它送进 TryProactive，也只能走直答链路，不能先问裁判要不要回。
func TestProactive点名消息直接回答(t *testing.T) {
	cfg := proactiveTestConfig()
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetReply("收到，马上说")
	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "在吗"},
	)

	event := proactiveEvent("u1", "m1", "在吗", onebot.MentionSegment("bot-openid"))
	instance.TryProactive(context.Background(), event, "g1", "@bot(bot-openid) 在吗")

	waitFor(t, "直接生成回复", func() bool { return len(fake.Calls()) == 1 })
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}
	if got := len(fake.JSONCalls()); got != 0 {
		t.Fatalf("点名消息不应进入裁判，实际调用 %d 次", got)
	}
}

func TestProactive纯图片在无看图能力时跳过(t *testing.T) {
	cfg := proactiveTestConfig()
	cfg.ProactiveJudgeImages = 0
	instance, fake := newProactiveTestRouter(t, cfg)
	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "[图片]"},
	)

	event := proactiveEvent("u1", "m1", "[图片]", onebot.ImageSegment("http://example.com/a.png"))
	instance.TryProactive(context.Background(), event, "g1", "[图片]")
	if got := len(fake.JSONCalls()); got != 0 {
		t.Fatalf("关闭看图时纯图片消息不应进入裁判，实际 %d 次", got)
	}
}
