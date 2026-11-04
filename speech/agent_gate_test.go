package speech

import (
	"context"
	"strconv"
	"testing"

	"good-review-master/config"
	"good-review-master/onebot"
)

// gateGroupEvent 造一条群聊事件。#启动 / #关闭 的匹配链先判点名，
// 所以正文里必须带 @bot(u1) 前缀（和 command_run_test 里同一套写法）。
func gateGroupEvent(groupID, text string) onebot.Event {
	return onebot.Event{
		Type:       "message",
		DetailType: "group",
		GroupID:    groupID,
		UserID:     "1",
		Self:       onebot.Self{Platform: "qq", UserID: "bot"},
		MessageID:  strconv.Itoa(len(text)),
		Message:    []onebot.Segment{onebot.TextSegment(text)},
	}
}

// Test门控指令的关键字变体 关键字只注册到 `#启动` / `#关闭`，
// 后面跟的感叹号（全角/半角）、人格名都落进 extra 由 handler 洗掉。
// continues 是拓扑唯一需要的输入：#启动 继续往下走，#关闭 到此为止。
func Test门控指令的关键字变体(t *testing.T) {
	e := newSwitchTestEnv(t)

	cases := []struct {
		text      string
		keyword   string
		continues bool
	}{
		{"@bot(u1) #启动", keywordAgentStart, true},
		{"@bot(u1) #启动！", keywordAgentStart, true},
		{"@bot(u1) #启动!", keywordAgentStart, true},
		{"@bot(u1) #启动！ 鲸鱼娘", keywordAgentStart, true},
		{"@bot(u1) #启动！HRBP", keywordAgentStart, true},
		{"@bot(u1) #关闭", keywordAgentStop, false},
		{"@bot(u1) #关闭！", keywordAgentStop, false},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			event := gateGroupEvent(e.groupID, tc.text)
			keyword, continues, ok := e.router.MatchInternalCommand(event, event.PlainText(), true)
			if !ok || keyword != tc.keyword || continues != tc.continues {
				t.Fatalf("MatchInternalCommand = (%q, %v, %v)，want (%q, %v, true)",
					keyword, continues, ok, tc.keyword, tc.continues)
			}
		})
	}
}

// Test人格名清洗 前缀树匹配不消费关键字之后的内容，
// 所以「！ 鲸鱼娘」整段会作为 extra 进来；只剥前导标点与空白，内部原样保留。
func Test人格名清洗(t *testing.T) {
	cases := []struct {
		extra string
		want  string
	}{
		{"", ""},
		{"！", ""},
		{"！ ", ""},
		{"！ 鲸鱼娘", "鲸鱼娘"},
		{"!HRBP", "HRBP"},
		{"：温柔", "温柔"},
		{"，锐评下", "锐评下"},
		{"~～ 鲸鱼娘 ", "鲸鱼娘"},
		{"！ 鲸鱼 娘", "鲸鱼 娘"},
	}
	for _, tc := range cases {
		if got := personaNameFromExtra(tc.extra); got != tc.want {
			t.Fatalf("personaNameFromExtra(%q) = %q，want %q", tc.extra, got, tc.want)
		}
	}
}

// Test启动打开开关并绑人格 一条指令做两件事：开关先开、人格后绑。
// 顺序是有意的：人格名写错时开关仍然要开——用户的意思是让这个群恢复运行。
func Test启动打开开关并绑人格(t *testing.T) {
	e := newSwitchTestEnv(t)
	if e.router.AgentEnabled(e.groupID) {
		t.Fatal("测试配置没开 agent_default_enabled，初始应当是关着的")
	}

	start := gateGroupEvent(e.groupID, "@bot(u1) #启动！ 锐评下")
	if !e.router.RunInternalCommand(context.Background(), start, e.groupID, start.PlainText(), true) {
		t.Fatal("#启动 应当被执行")
	}
	if !e.router.AgentEnabled(e.groupID) {
		t.Fatal("#启动 之后开关应当打开")
	}
	binding, ok := e.router.getGroupPersona(e.groupID)
	if !ok || binding.keyword != "锐评下" {
		t.Fatalf("#启动！ 锐评下 应当绑定该人格，ok=%v binding=%+v", ok, binding)
	}

	// #关闭 只关开关，不动人格：下次 #启动 不写名字就接着用这个人格。
	stop := gateGroupEvent(e.groupID, "@bot(u1) #关闭！")
	if !e.router.RunInternalCommand(context.Background(), stop, e.groupID, stop.PlainText(), true) {
		t.Fatal("#关闭 应当被执行")
	}
	if e.router.AgentEnabled(e.groupID) {
		t.Fatal("#关闭 之后开关应当关掉")
	}
	if _, ok := e.router.getGroupPersona(e.groupID); !ok {
		t.Fatal("#关闭 不应清空人格")
	}
}

// Test启动找不到人格就跳过加载 人格名不存在不是错误：
// 开关照开、人格保持原样，群不能因为写错一个名字就起不来。
func Test启动找不到人格就跳过加载(t *testing.T) {
	e := newSwitchTestEnv(t)

	first := gateGroupEvent(e.groupID, "@bot(u1) #启动 锐评下")
	e.router.RunInternalCommand(context.Background(), first, e.groupID, first.PlainText(), true)

	// 再把开关关掉，用一个人格名里根本没有的关键字重新启动。
	stop := gateGroupEvent(e.groupID, "@bot(u1) #关闭")
	e.router.RunInternalCommand(context.Background(), stop, e.groupID, stop.PlainText(), true)

	again := gateGroupEvent(e.groupID, "@bot(u1) #启动！ 不存在的角色")
	if !e.router.RunInternalCommand(context.Background(), again, e.groupID, again.PlainText(), true) {
		t.Fatal("#启动 应当被执行")
	}
	if !e.router.AgentEnabled(e.groupID) {
		t.Fatal("人格加载失败时开关仍然要开")
	}
	binding, ok := e.router.getGroupPersona(e.groupID)
	if !ok || binding.keyword != "锐评下" {
		t.Fatalf("找不到人格时应沿用当前人格，ok=%v binding=%+v", ok, binding)
	}
}

// Test会话开关缺省回落配置 内存里没有这条会话的记录时，
// 由 bot.agent_default_enabled 决定；显式设置过一次之后就以记录为准。
func Test会话开关缺省回落配置(t *testing.T) {
	on := &Router{
		agentGate: map[string]bool{},
		appCfg:    staticSnapshot(&config.Config{AgentDefaultEnabled: true}),
	}
	if !on.AgentEnabled("g1") {
		t.Fatal("没设置过时应回落到 agent_default_enabled=true")
	}
	on.SetAgentEnabled("g1", false)
	if on.AgentEnabled("g1") {
		t.Fatal("显式关过之后不应再回落成默认值")
	}

	off := &Router{
		agentGate: map[string]bool{},
		appCfg:    staticSnapshot(&config.Config{}),
	}
	if off.AgentEnabled("g1") {
		t.Fatal("没设置过且默认关闭时应当是关着的")
	}
}

// fakeGateStore 记录会话开关的落库调用。
type fakeGateStore struct {
	saved  map[string]bool
	called int
}

func (f *fakeGateStore) SaveGroupGate(chatID string, enabled bool) error {
	f.called++
	f.saved[chatID] = enabled
	return nil
}

// Test会话开关落库 #启动 / #关闭 不能只在内存里生效：
// 注入了 store 就必须写下去，重启后恢复才认得这个群被关过。
func Test会话开关落库(t *testing.T) {
	e := newSwitchTestEnv(t)
	store := &fakeGateStore{saved: make(map[string]bool)}
	e.router.SetAgentGateStore(store)

	start := gateGroupEvent(e.groupID, "@bot(u1) #启动")
	e.router.RunInternalCommand(context.Background(), start, e.groupID, start.PlainText(), true)
	if enabled, ok := store.saved[e.groupID]; !ok || !enabled {
		t.Fatalf("#启动 应当落库为开，saved=%+v", store.saved)
	}

	stop := gateGroupEvent(e.groupID, "@bot(u1) #关闭")
	e.router.RunInternalCommand(context.Background(), stop, e.groupID, stop.PlainText(), true)
	if enabled, ok := store.saved[e.groupID]; !ok || enabled {
		t.Fatalf("#关闭 应当落库为关，saved=%+v", store.saved)
	}
	if store.called != 2 {
		t.Fatalf("两次指令应当各写一次库，实际 %d 次", store.called)
	}
}

// Test恢复会话开关 启动时读回来的值直接生效，且不回写数据库
// （回写只会白抬 settings 的自增号）。
func Test恢复会话开关(t *testing.T) {
	store := &fakeGateStore{saved: make(map[string]bool)}
	r := &Router{
		agentGate:      map[string]bool{},
		appCfg:         staticSnapshot(&config.Config{AgentDefaultEnabled: true}),
		agentGateStore: store,
	}
	r.RestoreAgentGates(map[string]bool{"g1": false, "g2": true})

	if r.AgentEnabled("g1") {
		t.Fatal("恢复后 g1 应当是关着的")
	}
	if !r.AgentEnabled("g2") {
		t.Fatal("恢复后 g2 应当是开着的")
	}
	if store.called != 0 {
		t.Fatalf("恢复不应回写数据库，实际写了 %d 次", store.called)
	}
}
