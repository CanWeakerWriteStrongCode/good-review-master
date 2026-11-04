package speech

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"good-review-master/config"
	"good-review-master/conversation"
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

// Test人格名前置的启动写法 #鲸鱼娘，启动！ / #HRBP,启动!
// 前缀树存不了任意人格名，所以它是一条跑在 trie 之前的专用规则，命中后复用 #启动 那条路由。
// 形状是硬的：# 紧接名字、名字紧接逗号、逗号紧接启动、启动紧接感叹号，四处都不许插空格。
func Test人格名前置的启动写法(t *testing.T) {
	e := newSwitchTestEnv(t)

	cases := []struct {
		text string
		name string
		hit  bool
	}{
		{"@bot(u1) #鲸鱼娘，启动！", "鲸鱼娘", true},
		{"@bot(u1) #HRBP,启动!", "HRBP", true},
		// 全角/半角混搭也认：逗号与感叹号各自两种。
		{"@bot(u1) #鲸鱼娘,启动！", "鲸鱼娘", true},
		// 触发词后面跟别的内容：命中，后面的字不参与解析。
		{"@bot(u1) #鲸鱼娘，启动！ 今天怎么样", "鲸鱼娘", true},
		// 旧写法走前缀树，extra 是未洗过的尾巴，由 handler 自己洗。
		{"@bot(u1) #启动！ 鲸鱼娘", "！ 鲸鱼娘", true},
		// 下面都不是指令：四处必须连写，一个字都不能多。
		{"@bot(u1) #鲸鱼娘 ，启动！", "", false},      // 名字后空格
		{"@bot(u1) #鲸鱼娘， 启动！", "", false},      // 逗号后空格
		{"@bot(u1) #鲸鱼娘，启动 ！", "", false},      // 启动后空格
		{"@bot(u1) # 鲸鱼娘，启动！", "", false},      // 井号后空格
		{"@bot(u1) #鲸鱼 娘，启动！", "", false},      // 名字中间空格
		{"@bot(u1) #鲸鱼娘\u3000，启动！", "", false}, // 全角空格也不惯着
		{"@bot(u1) #，启动！", "", false},          // 名字不能为空
		{"@bot(u1) #鲸鱼娘，启动", "", false},        // 感叹号不能省
		{"@bot(u1) #鲸鱼娘，关闭！", "", false},       // 关闭不做对称
		{"@bot(u1) #鲸鱼娘启动！", "", false},        // 没有逗号
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			event := gateGroupEvent(e.groupID, tc.text)
			route, name, ok := e.router.matchInternal(event, event.PlainText(), true)
			if ok != tc.hit {
				t.Fatalf("matchInternal ok=%v，want %v（route=%+v）", ok, tc.hit, route)
			}
			if !ok {
				return
			}
			// 复用 #启动 那一份：ContinueAfterRun 与 handler 都不能再多存一份。
			if route.Keyword != keywordAgentStart || !route.ContinueAfterRun {
				t.Fatalf("应当复用 #启动 那条路由，实际 %+v", route)
			}
			if name != tc.name {
				t.Fatalf("人格名 = %q，want %q", name, tc.name)
			}
		})
	}
}

// lastSelfReply 取群聊缓存里机器人自己的最后一条发言。
// 测试里的发送地址是死的（回执发不出去），但 recordSelfReply 会把它记进缓存——
// 那就是群里看到的那段文字。
func lastSelfReply(t *testing.T, e *switchTestEnv) string {
	t.Helper()
	msgs := conversation.GetGroupCache(e.groupID, 100).GetAll()
	for i := len(msgs) - 1; i >= 0; i-- {
		if strings.HasPrefix(msgs[i].MessageID, "bot-") {
			return msgs[i].Content
		}
	}
	t.Fatal("缓存里没有机器人自己的发言")
	return ""
}

// Test启动发的是人格字符画 启动回执不再是一句「已启动：打标签与自动回复已开启」，
// 而是「引线 + 人格字符画 + 一行 <人格名>正在启动中...」。
// 引线不能省：回执是「引用 + @ + 正文」，@ 会和正文首行拼在同一行，
// 没这行垫着它就挤进字符画首行，整张图当场散架（群里实测过）。
func Test启动发的是人格字符画(t *testing.T) {
	e := newSwitchTestEnv(t)

	start := gateGroupEvent(e.groupID, "@bot(u1) #启动！ 锐评下")
	if !e.router.RunInternalCommand(context.Background(), start, e.groupID, start.PlainText(), true) {
		t.Fatal("#启动 应当被执行")
	}
	want := bannerRule + "\n(｡•̀ᴗ-)✧\n── 毒舌 ──\n锐评下正在启动中..."
	if got := lastSelfReply(t, e); got != want {
		t.Fatalf("启动回执 = %q，want %q", got, want)
	}
}

// Test字符画的行首空格会被抹掉 QQ 会把消息里每行开头的空白吃掉，
// 靠缩进摆位的图在群里必定散架。发之前就抹平，配置里怎么写、群里就怎么显示。
func Test字符画的行首空格会被抹掉(t *testing.T) {
	got := trimArtIndent("    ⌇ ⌇\n( ˶•ᴗ•˶ )>~ 💙\n ╰─────╯\n\n")
	want := "⌇ ⌇\n( ˶•ᴗ•˶ )>~ 💙\n╰─────╯"
	if got != want {
		t.Fatalf("trimArtIndent = %q，want %q", got, want)
	}
	if got := trimArtIndent(" \n\t\n"); got != "" {
		t.Fatalf("全是空白的 art 应为空串，实际 %q", got)
	}
}

// Test切换人格与启动共用同一套上线话术 两边说的都是「这个人格上线了」，
// 所以出口只有一个（personaBanner）；分成两套文案迟早漂。
func Test切换人格与启动共用同一套上线话术(t *testing.T) {
	e := newSwitchTestEnv(t)

	// 先开闸门：关着换人格走的是「已切换（本群关闭中…）」，见下一则用例。
	start := gateGroupEvent(e.groupID, "@bot(u1) #启动")
	if !e.router.RunInternalCommand(context.Background(), start, e.groupID, start.PlainText(), true) {
		t.Fatal("#启动 应当被执行")
	}

	sw := gateGroupEvent(e.groupID, "@bot(u1) #切换人格 锐评下")
	if !e.router.RunInternalCommand(context.Background(), sw, e.groupID, sw.PlainText(), true) {
		t.Fatal("#切换人格 应当被执行")
	}
	want := bannerRule + "\n(｡•̀ᴗ-)✧\n── 毒舌 ──\n锐评下正在启动中..."
	if got := lastSelfReply(t, e); got != want {
		t.Fatalf("切人格回执 = %q，want %q", got, want)
	}

	// 不带人格名的 #启动 沿用当前人格：发的还是它那张脸，不是系统话术。
	again := gateGroupEvent(e.groupID, "@bot(u1) #启动")
	if !e.router.RunInternalCommand(context.Background(), again, e.groupID, again.PlainText(), true) {
		t.Fatal("#启动 应当被执行")
	}
	if got := lastSelfReply(t, e); got != want {
		t.Fatalf("沿用当前人格时的回执 = %q，want %q", got, want)
	}
}

// Test没有人格字符画就只发一行 字符画是可选的：人格没配 art 时只发启动语，
// 不能因此退回那句生硬的系统话术。
func Test没有人格字符画就只发一行(t *testing.T) {
	e := newSwitchTestEnv(t)

	start := gateGroupEvent(e.groupID, "@bot(u1) #启动")
	e.router.RunInternalCommand(context.Background(), start, e.groupID, start.PlainText(), true)

	sw := gateGroupEvent(e.groupID, "@bot(u1) #切换人格 温柔")
	if !e.router.RunInternalCommand(context.Background(), sw, e.groupID, sw.PlainText(), true) {
		t.Fatal("#切换人格 应当被执行")
	}
	if got := lastSelfReply(t, e); got != bannerRule+"\n温柔正在启动中..." {
		t.Fatalf("无字符画时回执 = %q，want %q", got, bannerRule+"\n温柔正在启动中...")
	}
}

// Test没有人格时启动报机器人自己的名字 一个人格都没设过的群：上线的是机器人本人，
// 报它的昵称。不再退回「✅ 已启动：打标签与自动回复已开启」那种系统话术。
func Test没有人格时启动报机器人自己的名字(t *testing.T) {
	e := newSwitchTestEnv(t)

	start := gateGroupEvent(e.groupID, "@bot(u1) #启动")
	if !e.router.RunInternalCommand(context.Background(), start, e.groupID, start.PlainText(), true) {
		t.Fatal("#启动 应当被执行")
	}
	want := bannerRule + "\nbot正在启动中..." // 昵称来自 switchTestEnv 的 BotNickname
	if got := lastSelfReply(t, e); got != want {
		t.Fatalf("无人格时的启动回执 = %q，want %q", got, want)
	}
}

// Test启动人格名写错只说没找到 名字不存在时只留一句提示：
// 不再顺带一句「已启动：…已开启」，也不发字符画（那会让群里以为真换上了）。
func Test启动人格名写错只说没找到(t *testing.T) {
	e := newSwitchTestEnv(t)

	start := gateGroupEvent(e.groupID, "@bot(u1) #启动！ 不存在的人格")
	e.router.RunInternalCommand(context.Background(), start, e.groupID, start.PlainText(), true)
	want := "❌ 未找到人格「不存在的人格」，本群还没设定人格。"
	if got := lastSelfReply(t, e); got != want {
		t.Fatalf("写错名字的回执 = %q，want %q", got, want)
	}
	// 开关照开：用户的意思是「让这个群恢复运行」，人格只是顺带的修饰。
	if !e.router.AgentEnabled(e.groupID) {
		t.Fatal("人格没找到也要把开关打开")
	}

	// 已经有人格时，要说明这句话仍由谁回。
	sw := gateGroupEvent(e.groupID, "@bot(u1) #切换人格 锐评下")
	e.router.RunInternalCommand(context.Background(), sw, e.groupID, sw.PlainText(), true)
	again := gateGroupEvent(e.groupID, "@bot(u1) #启动 不存在的人格")
	e.router.RunInternalCommand(context.Background(), again, e.groupID, again.PlainText(), true)
	want = "❌ 未找到人格「不存在的人格」，仍由「锐评下」回应。"
	if got := lastSelfReply(t, e); got != want {
		t.Fatalf("写错名字的回执 = %q，want %q", got, want)
	}
}

// Test关着的时候换人格会说明原因 #切换人格 在 #关闭 的群里照旧执行成功，
// 但 agent 那一跳会被开关静默丢掉——回执必须把这件事说清楚，
// 否则群里只看到一张脸和一句“正在启动中”，会以为切失败了。
func Test关着的时候换人格会说明原因(t *testing.T) {
	e := newSwitchTestEnv(t)
	if e.router.AgentEnabled(e.groupID) {
		t.Fatal("测试配置没开 agent_default_enabled，初始应当是关着的")
	}

	sw := gateGroupEvent(e.groupID, "@bot(u1) #切换人格 锐评下")
	if !e.router.RunInternalCommand(context.Background(), sw, e.groupID, sw.PlainText(), true) {
		t.Fatal("#切换人格 应当被执行")
	}
	want := bannerRule + "\n(｡•̀ᴗ-)✧\n── 毒舌 ──\n锐评下已切换（本群关闭中，暂时不会回应，发 " + keywordAgentStart + " 可开启）"
	if got := lastSelfReply(t, e); got != want {
		t.Fatalf("关着时的切人格回执 = %q，want %q", got, want)
	}
	// 关着只是不回应，不是不记账：人格确实换上了。
	if b, ok := e.router.getGroupPersona(e.groupID); !ok || b.keyword != "锐评下" {
		t.Fatalf("人格应当已切换，ok=%v binding=%+v", ok, b)
	}
}

// Test人格名前置真的能绑上人格 端到端把名字交给 handler 的那一跳：
// 不是只「命中」，而是开关开了、人格换成它。
func Test人格名前置真的能绑上人格(t *testing.T) {
	e := newSwitchTestEnv(t)

	event := gateGroupEvent(e.groupID, "@bot(u1) #锐评下，启动！")
	if !e.router.RunInternalCommand(context.Background(), event, e.groupID, event.PlainText(), true) {
		t.Fatal("#锐评下，启动！ 应当被执行")
	}
	if !e.router.AgentEnabled(e.groupID) {
		t.Fatal("开关应当打开")
	}
	binding, ok := e.router.getGroupPersona(e.groupID)
	if !ok || binding.keyword != "锐评下" {
		t.Fatalf("应当绑上锐评下人格，ok=%v binding=%+v", ok, binding)
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
