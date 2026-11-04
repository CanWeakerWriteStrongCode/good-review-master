package speech

import (
	"context"
	"testing"

	"good-review-master/config"
	"good-review-master/onebot"
)

// TestCanUseInternalCommand 权限判定的分支：#帮助 与功能指令不受限，私聊放行，
// 群里的名单外成员一律拒绝。
func TestCanUseInternalCommand(t *testing.T) {
	r := &Router{}
	cfg := &config.Config{CommandAdmins: map[string][]string{"g1": {"u1"}}}

	admin := &Command{Keyword: "#切换人格", Category: "internal"}
	help := &Command{Keyword: internalKeywordHelp, Category: "internal"}
	feature := &Command{Keyword: "锐评下", Category: "chat_review"}

	cases := []struct {
		name   string
		route  *Command
		chatID string
		userID string
		want   bool
	}{
		{"名单内管理员", admin, "g1", "u1", true},
		{"名单外成员", admin, "g1", "u2", false},
		{"没配的群", admin, "g2", "u1", false},
		{"#帮助 对所有人开放", help, "g1", "u2", true},
		{"功能指令不受限", feature, "g1", "u2", true},
		{"私聊直接放行", admin, "private:u9", "u9", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := r.canUseInternalCommand(cfg, tc.route, tc.chatID, tc.userID); got != tc.want {
				t.Fatalf("canUseInternalCommand = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestMatchInternalCommand只认管理指令 分流只认 Category == internal：
// 功能指令与普通聊天都留在数据平面。
func TestMatchInternalCommand只认管理指令(t *testing.T) {
	e := newSwitchTestEnv(t)
	// 指令解析现在只看事件正文（文本段），所以每个用例单独造事件，
	// 不能再靠传一份与事件不一致的 content 来模拟不同消息。
	group := func(text string) onebot.Event {
		return onebot.Event{
			Type:       "message",
			DetailType: "group",
			GroupID:    e.groupID,
			UserID:     "1",
			Self:       onebot.Self{Platform: "qq", UserID: "bot"},
			Message:    []onebot.Segment{onebot.TextSegment(text)},
		}
	}

	// #切换人格 切成功后要往下走：这句话本身要用刚换来的人格回。
	// 没切成的三种分支由 ContinueWhen 拦住，见 Test切换失败不往下走。
	cmd := group("@bot(u1) #切换人格 锐评下")
	if keyword, continues, ok := e.router.MatchInternalCommand(cmd, cmd.PlainText(), true); !ok ||
		keyword != "#切换人格" || !continues {
		t.Fatalf("应命中 #切换人格 且继续，got %q continues=%v ok=%v", keyword, continues, ok)
	}
	// 不需要后续动作的指令仍然到此为止，不能多跑一跳。
	help := group("@bot(u1) #帮助")
	if keyword, continues, ok := e.router.MatchInternalCommand(help, help.PlainText(), true); !ok ||
		keyword != internalKeywordHelp || continues {
		t.Fatalf("#帮助 应命中且终止，got %q continues=%v ok=%v", keyword, continues, ok)
	}
	feature := group("@bot(u1) 锐评下 这台机器")
	if _, _, ok := e.router.MatchInternalCommand(feature, feature.PlainText(), true); ok {
		t.Fatal("功能指令不应进入指令域")
	}
	chat := group("@bot(u1) 今天天气不错")
	if _, _, ok := e.router.MatchInternalCommand(chat, chat.PlainText(), true); ok {
		t.Fatal("普通聊天不应进入指令域")
	}
}

// Test指令参数不受尾部at污染 真实消息里 @ 可能排在正文后面：把渲染后的正文
// 「#切换人格@昵称(ID)」当参数，就会得到「未找到人格：@昵称(ID)」。
func Test指令参数不受尾部at污染(t *testing.T) {
	e := newSwitchTestEnv(t)
	event := onebot.Event{
		Type:       "message",
		DetailType: "group",
		GroupID:    e.groupID,
		UserID:     "1",
		Self:       onebot.Self{Platform: "qq", UserID: "bot"},
		Message: []onebot.Segment{
			onebot.TextSegment("#切换人格"),
			onebot.MentionSegment("bot"),
		},
	}

	route, extra, ok := e.router.matchInternal(event, "#切换人格@bot(bot)", true)
	if !ok || route.Keyword != "#切换人格" {
		t.Fatalf("应命中 #切换人格，ok=%v route=%+v", ok, route)
	}
	if extra != "" {
		t.Fatalf("尾部 @ 不应进入参数，实际参数 %q", extra)
	}
}

// Test切换人格失败不往下走 三种没切成的分支（只列可选人格/名字不存在/关键字没人格）
// 必须到此为止：否则群里先看到报错，再收到一句莫名其妙的应答。
func Test切换人格失败不往下走(t *testing.T) {
	e := newSwitchTestEnv(t)

	cases := []struct {
		name string
		text string
	}{
		{"只列可选人格", "@bot(u1) #切换人格"},
		{"名字不存在", "@bot(u1) #切换人格 不存在的角色"},
		{"关键字没人格", "@bot(u1) #切换人格 闲聊"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event := onebot.Event{
				Type:       "message",
				DetailType: "group",
				GroupID:    e.groupID,
				UserID:     "1",
				Self:       onebot.Self{Platform: "qq", UserID: "bot"},
				Message:    []onebot.Segment{onebot.TextSegment(tc.text)},
			}
			keyword, continues, ok := e.router.MatchInternalCommand(event, event.PlainText(), true)
			if !ok || keyword != "#切换人格" || continues {
				t.Fatalf("MatchInternalCommand = (%q, %v, %v)，want (#切换人格, false, true)",
					keyword, continues, ok)
			}
		})
	}
}

// TestRunInternalCommand未授权不产生副作用 名单外成员执行 #切换人格 只收到拒绝，
// 群人格保持为空——提示语走发送链（测试里是死地址，只打日志），所以用状态断言。
func TestRunInternalCommand未授权不产生副作用(t *testing.T) {
	e := newSwitchTestEnv(t)
	event := onebot.Event{
		Type:       "message",
		DetailType: "group",
		GroupID:    e.groupID,
		UserID:     "2",
		Self:       onebot.Self{Platform: "qq", UserID: "bot"},
		Message:    []onebot.Segment{onebot.TextSegment("@bot(u2) #切换人格 锐评下")},
	}

	if ok := e.router.RunInternalCommand(context.Background(), event, e.groupID,
		"@bot(u2) #切换人格 锐评下", true); !ok {
		t.Fatal("命中指令应返回 true（已处理，不再往下游投递）")
	}
	if b, exists := e.router.getGroupPersona(e.groupID); exists {
		t.Fatalf("未授权用户不应改变群人格：%+v", b)
	}
}
