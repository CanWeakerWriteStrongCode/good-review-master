package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"good-review-master/config"
	"good-review-master/conversation"
	"good-review-master/internal/testutil"
	"good-review-master/onebot"
	"good-review-master/workspace"
)

// gateTestYAML 一条带人格的关键字：#启动！ 锐评下 要能把「毒舌点评人」绑到这个群上，
// 断言的方式就是看模型收到的 systemPrompt 里有没有它。
const gateTestYAML = `cmd:
  chat_review:
    - keyword: "锐评下"
      prompt: |
        做犀利总结。
      persona:
        identity: |
          毒舌点评人
        personality: '["一针见血"]'
        relationship: '{"角色":"点评员"}'
        system_prompt: |
          守住底线
        emotion: '{"情绪反应性":"高"}'
rules:
  chat_review: |
    规则：禁止人身攻击
`

type gateTestEnv struct {
	bot    *Bot
	mgr    *workspace.Manager
	fake   *testutil.FakeLLM
	group  string
	opened *workspace.Workspace // 第一次入站之后才开通，缓存在这里
}

// newGateTestEnv 起一个真实装配的机器人：前置 → command → assets → agent 全在这条链上。
// mutate 用来拨配置项（默认值、管理员名单），不与其它用例共用一份 Config。
func newGateTestEnv(t *testing.T, mutate func(*config.Config)) *gateTestEnv {
	t.Helper()

	dir := t.TempDir()
	system := filepath.Join(dir, "prompt_system.yaml")
	if err := os.WriteFile(system, []byte(gateTestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		AllowGroups:       []string{"g1"},
		WorkspaceAccounts: []string{"qq_official_bot_1906"},
		BotNickname:       "小助理",
		// 直接构造的配置不走 SetDefaults：默认关着，#启动 才有东西可开。
		AgentDefaultEnabled: false,
		CommandAdmins:       map[string][]string{"g1": {"u1"}},
		MaxCacheMsg:         100,
		MaxMsgRune:          500,
		LLMSendCount:        10,
		LLMTimeout:          2 * time.Second,
	}
	if mutate != nil {
		mutate(cfg)
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

	return &gateTestEnv{bot: b, mgr: mgr, fake: fake, group: "g1"}
}

// workspace 返回本账号的工作空间。它是慵懒开通的：第一条消息进来才建，
// 所以在 send 之前取不到——测试里每次用完即取，不把指针存成字段。
func (e *gateTestEnv) workspace(t *testing.T) *workspace.Workspace {
	t.Helper()
	if e.opened != nil {
		return e.opened
	}
	ws, ok := e.mgr.ByID("qq_official_bot_1906")
	if !ok {
		t.Fatal("工作空间未开通")
	}
	e.opened = ws
	return ws
}

// send 以管理员身份发一条群消息。
func (e *gateTestEnv) send(t *testing.T, msgID, text string) {
	t.Helper()
	event := onebot.Event{
		ID:          "e-" + msgID,
		Time:        float64(time.Now().Unix()),
		Type:        "message",
		DetailType:  "group",
		Self:        onebot.Self{Platform: "qq", UserID: "bot-openid"},
		AccountKind: "official_bot",
		MessageID:   msgID,
		GroupID:     e.group,
		UserID:      "u1",
		Message: []onebot.Segment{
			onebot.TextSegment(text),
			onebot.MentionSegment("bot-openid"),
		},
		AltMessage: "@小助理 " + text,
	}
	if accepted, err := e.bot.AcceptEvent(context.Background(), "1906", event); err != nil || !accepted {
		t.Fatalf("AcceptEvent = %v, %v", accepted, err)
	}
}

// waitCalls 等模型被调用到 want 次；等不到就由调用方决定怎么报错。
func (e *gateTestEnv) waitCalls(t *testing.T, want int) []testutil.LLMCall {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if calls := e.fake.Calls(); len(calls) >= want {
			return calls
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("大模型调用数未达预期: want>=%d got=%d", want, len(e.fake.Calls()))
	return nil
}

// waitCached 等这条消息进群聊缓存（agent 处理过的证据）。
func (e *gateTestEnv) waitCached(t *testing.T, msgID string) []conversation.Message {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		msgs := e.workspace(t).Cache.GetGroupCache(e.group, 100).GetAll()
		if conversation.FindMsgIndex(msgs, msgID) >= 0 {
			return msgs
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("消息 %s 没有进入群聊缓存：%+v", msgID,
		e.workspace(t).Cache.GetGroupCache(e.group, 100).GetAll())
	return nil
}

// Test启动指令开闸门之后这句话仍被回复 整个改造的核心验收：
// 一条 @机器人 #启动！<人格名> 既要打开开关，又要紧接着用刚绑好的那个人格回一句。
//
// 断言三件事：开关真的开了并落了库；模型被调了一次（不是零次——那说明被指令吞了；
// 也不是两次——那说明指令执行了两遍）；回话用的是刚绑的人格。
func Test启动指令开闸门之后这句话仍被回复(t *testing.T) {
	e := newGateTestEnv(t, nil)

	e.send(t, "m1", "#启动！ 锐评下")

	calls := e.waitCalls(t, 1)
	if len(calls) != 1 {
		t.Fatalf("这条门控指令应当只生成一次回复，实际 %d 次", len(calls))
	}
	if !strings.Contains(calls[0].SystemPrompt, "毒舌点评人") {
		t.Fatalf("#启动！ 锐评下 之后应当用这个人格回复：\n%s", calls[0].SystemPrompt)
	}
	ws := e.workspace(t)
	if !ws.Router.AgentEnabled(e.group) {
		t.Fatal("#启动 之后开关应当是开着的")
	}
	// 开关要落到共享的 state.db：重启之后才认得这个群被显式开过。
	gates, err := ws.State.LoadGroupGates()
	if err != nil {
		t.Fatalf("LoadGroupGates: %v", err)
	}
	if enabled, ok := gates[e.group]; !ok || !enabled {
		t.Fatalf("#启动 应当落库为开，实际 %+v", gates)
	}
	// 它同时又是一条普通消息：进缓存，模型才看得到"刚刚有人让我启动"。
	e.waitCached(t, "m1")
}

// Test关闭指令只回执不往下走 #关闭 是终止型指令：
// 开关关上，这条消息不进打标签/生成，也不调模型——回执由指令自己发。
func Test关闭指令只回执不往下走(t *testing.T) {
	e := newGateTestEnv(t, func(cfg *config.Config) { cfg.AgentDefaultEnabled = true })

	e.send(t, "m1", "#关闭！")

	// 工作空间是第一条消息进来才开通的，所以先发再取。
	ws := e.workspace(t)

	// 给链路足够时间跑完；真走错的话这会儿已经调过模型了。
	time.Sleep(500 * time.Millisecond)
	if calls := e.fake.Calls(); len(calls) != 0 {
		t.Fatalf("#关闭 不应调用大模型，实际 %d 次", len(calls))
	}
	if ws.Router.AgentEnabled(e.group) {
		t.Fatal("#关闭 之后开关应当是关着的")
	}
	gates, err := ws.State.LoadGroupGates()
	if err != nil {
		t.Fatalf("LoadGroupGates: %v", err)
	}
	if enabled, ok := gates[e.group]; !ok || enabled {
		t.Fatalf("#关闭 应当落库为关，实际 %+v", gates)
	}
	// 回执本身是机器人自己说的话，会进缓存；不该在里面的是那条指令本体。
	if msgs := ws.Cache.GetGroupCache(e.group, 100).GetAll(); conversation.FindMsgIndex(msgs, "m1") >= 0 {
		t.Fatalf("终止型指令本体不应进入群聊缓存：%+v", msgs)
	}
}

// Test会话关着时静默入库 开关关掉之后照常入库（历史不能丢），
// 但一句都不回：群里已经让它闭嘴了，再回一句"我关着"正是该停的噪音。
func Test会话关着时静默入库(t *testing.T) {
	e := newGateTestEnv(t, nil)

	e.send(t, "m1", "你好")

	e.waitCached(t, "m1")
	time.Sleep(200 * time.Millisecond)
	if calls := e.fake.Calls(); len(calls) != 0 {
		t.Fatalf("会话关着时不应调用大模型，实际 %d 次", len(calls))
	}
}

// Test非管理员启动不了 #启动 是管理指令：名单外成员只会收到拒绝，
// 开关不动，也没有生成。
func Test非管理员启动不了(t *testing.T) {
	e := newGateTestEnv(t, nil)

	event := onebot.Event{
		ID:          "e-m1",
		Time:        float64(time.Now().Unix()),
		Type:        "message",
		DetailType:  "group",
		Self:        onebot.Self{Platform: "qq", UserID: "bot-openid"},
		AccountKind: "official_bot",
		MessageID:   "m1",
		GroupID:     e.group,
		UserID:      "u2",
		Message: []onebot.Segment{
			onebot.TextSegment("#启动"),
			onebot.MentionSegment("bot-openid"),
		},
		AltMessage: "@小助理 #启动",
	}
	if accepted, err := e.bot.AcceptEvent(context.Background(), "1906", event); err != nil || !accepted {
		t.Fatalf("AcceptEvent = %v, %v", accepted, err)
	}

	time.Sleep(500 * time.Millisecond)
	if e.workspace(t).Router.AgentEnabled(e.group) {
		t.Fatal("未授权用户的 #启动 不应打开开关")
	}
	if calls := e.fake.Calls(); len(calls) != 0 {
		t.Fatalf("被拒绝的指令不应走到生成，实际 %d 次", len(calls))
	}
}
