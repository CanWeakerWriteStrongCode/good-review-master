package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/onebot"
)

// TestMain 初始化日志并切到临时目录：发送路径会打日志，未初始化会 nil panic。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "router-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(dir); err != nil {
		panic(err)
	}
	logutil.SetupLogger()
	code := m.Run()
	logutil.Close()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func TestComposeSystemPrompt包含稳定身份与at约定(t *testing.T) {
	cfg := &config.Config{BotNickname: "不是好评大师", LLMConfig: config.LLMConf{ModelName: "m"}}

	prompt := ComposeSystemPrompt(cfg, "0C2F9A2A", "", "【要求】测试", "")
	if !strings.Contains(prompt, "你的昵称是【不是好评大师】，用户ID是【0C2F9A2A】") {
		t.Fatalf("提示词应同时给出昵称与稳定 ID：%s", prompt)
	}
	if !strings.Contains(prompt, "user_id 是发言者的稳定ID") || !strings.Contains(prompt, "@昵称(ID)") {
		t.Fatalf("提示词应说明 @昵称(ID) 与 user_id 的约定：%s", prompt)
	}
	if !strings.Contains(prompt, "message_id 是这条消息的编号") || !strings.Contains(prompt, "nickname 是昵称") {
		t.Fatalf("提示词应说明与 OneBot 对齐的字段名：%s", prompt)
	}
	if !strings.Contains(prompt, "reply_id") {
		t.Fatalf("提示词应说明 reply_id 的含义：%s", prompt)
	}
}

func TestStripCQPrefix剥离各种at写法(t *testing.T) {
	cfg := &config.Config{BotNickname: "不是好评大师"}
	instance := &Router{}

	cases := []struct {
		name string
		raw  string
		want string
	}{
		{name: "昵称加 ID", raw: "@不是好评大师(0C2F9A2A) 锐评", want: "锐评"},
		{name: "多个 at 且机器人不在第一个", raw: "@张三(3319D7) @不是好评大师(0C2F9A2A) 锐评", want: "锐评"},
		{name: "手打昵称", raw: "@不是好评大师 锐评", want: "锐评"},
		{name: "保留正文里的其他 at", raw: "@不是好评大师(0C2F9A2A) 告诉@张三(3319D7)", want: "告诉@张三(3319D7)"},
		// 「@你」不做特殊剥离：提示词里已给出机器人昵称，交给模型自行判断
		{name: "不特殊处理@你", raw: "@你 锐评", want: "@你 锐评"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := instance.stripCQPrefix(tc.raw, cfg); got != tc.want {
				t.Fatalf("stripCQPrefix() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Test只at不说话的直答 只有 @ 没有正文（剥完前缀是空串）时不能丢，
// 仍走普通问答让模型自己回应；原始内容本身为空才保持丢弃。
func Test只at不说话的直答(t *testing.T) {
	cfg := proactiveTestConfig()
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetReply("怎么了")

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "@bot(bot-openid)"},
	)
	event := proactiveEvent("u1", "m1", "@bot(bot-openid)", onebot.MentionSegment("bot-openid"))
	instance.RouteMessage(context.Background(), "@bot(bot-openid)", event, "g1")

	waitFor(t, "生成调用", func() bool { return len(fake.Calls()) == 1 })
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}
	msgs := instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).GetAll()
	if len(msgs) != 2 || msgs[1].Content != "怎么了" {
		t.Fatalf("只 @ 不说话也应回复，实际 %+v", msgs)
	}
}

// Test只at别人不回复 剥完前缀没有正文、mention 里也不是本账号：直接丢弃，
// 不能因为"空正文"就把它当成一次点名。
func Test只at别人不回复(t *testing.T) {
	cfg := proactiveTestConfig()
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetReply("不该出现")

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "@用户(3319D7)"},
	)
	event := proactiveEvent("u1", "m1", "@用户(3319D7)", onebot.MentionSegment("3319D7"))
	instance.RouteMessage(context.Background(), "@用户(3319D7)", event, "g1")
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}

	if got := len(fake.Calls()); got != 0 {
		t.Fatalf("@ 别人不应生成回复，实际调用 %d 次", got)
	}
	if msgs := instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).GetAll(); len(msgs) != 1 {
		t.Fatalf("不应写入机器人回复，实际 %+v", msgs)
	}
}

// TestReply引用触发消息 回复要引用“别人 @ 我的那条”，而不是他引用的那条。
func TestReply引用触发消息(t *testing.T) {
	var segments []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Action string         `json:"action"`
			Params map[string]any `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
			return
		}
		if body.Action != "send_message" {
			t.Errorf("action = %q", body.Action)
			return
		}
		list, _ := body.Params["message"].([]any)
		for _, item := range list {
			segments = append(segments, item.(map[string]any))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "retcode": 0, "data": map[string]any{}})
	}))
	defer server.Close()

	router := &Router{obClient: onebot.NewClient(server.URL, nil, "").For("qq_test")}
	event := onebot.Event{
		MessageID: "ROBOT_NEW",
		UserID:    "u1",
		Message:   []onebot.Segment{onebot.ReplySegment("ROBOT_OLD")},
	}
	router.reply(event, "g1", "收到")

	if len(segments) != 3 {
		t.Fatalf("应有 reply+mention+text 三段，实际 %+v", segments)
	}
	if segments[0]["type"] != "reply" {
		t.Fatalf("第一段应是 reply：%+v", segments[0])
	}
	data, _ := segments[0]["data"].(map[string]any)
	if data["message_id"] != "ROBOT_NEW" {
		t.Fatalf("引用目标应是触发消息，实际 %v", data["message_id"])
	}
}

// Test直接at只回最后一条点名消息 群内被点名时：焦点提示换成点名版，
// 且窗口截止到触发消息——触发后才到达的消息不能进入本次请求。
func Test直接at只回最后一条点名消息(t *testing.T) {
	cfg := proactiveTestConfig()
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetReply("收到")

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "晚饭吃啥"},
		cache.Message{MessageID: "m2", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "@bot(bot-openid) 这个报错怎么解"},
		cache.Message{MessageID: "m3", GroupID: "g1", UserID: "u2", Nickname: "李四", Content: "后到的闲聊"},
	)

	content := "@bot(bot-openid) 这个报错怎么解"
	event := proactiveEvent("u1", "m2", content, onebot.MentionSegment("bot-openid"))
	instance.RouteMessage(context.Background(), content, event, "g1")
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("应只有一次生成调用，实际 %d", len(calls))
	}
	if !strings.Contains(calls[0].SystemPrompt, "只回复最后一条找你") {
		t.Fatalf("群内 @ 应使用点名焦点提示：%s", calls[0].SystemPrompt)
	}
	if !strings.Contains(calls[0].ChatLog, "这个报错怎么解") {
		t.Fatalf("触发消息本身应在窗口里：%s", calls[0].ChatLog)
	}
	if strings.Contains(calls[0].ChatLog, "后到的闲聊") {
		t.Fatalf("触发消息之后的消息不应进入窗口：%s", calls[0].ChatLog)
	}
}

// Test回传模式下不重复记录回复 实现端会把机器人自己的消息回传成事件时（NapCat），
// 入站链路已经写进缓存/DB；本地再记一次就是重复的两条。
func Test回传模式下不重复记录回复(t *testing.T) {
	cfg := proactiveTestConfig()
	cfg.EchoSelfMessages = true
	instance, _ := newProactiveTestRouter(t, cfg)
	var saved []cache.Message
	instance.SetMessageSink(func(m cache.Message) int64 { saved = append(saved, m); return 0 })

	event := onebot.Event{
		Type: "message", DetailType: "group", GroupID: "g1", UserID: "u1", MessageID: "m1",
		Self: onebot.Self{Platform: "qq", UserID: "bot-openid"},
	}
	instance.recordSelfReply(event, "g1", "收到")

	if got := instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).Len(); got != 0 {
		t.Fatalf("回传模式下本地不应再记回复，缓存里却有 %d 条", got)
	}
	if len(saved) != 0 {
		t.Fatalf("回传模式下不应再落库本地回复：%+v", saved)
	}

	// 关掉开关时仍然要记：当前链路（AstrBot 桥）不会回传。
	cfg.EchoSelfMessages = false
	instance.recordSelfReply(event, "g1", "收到")
	if got := instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).Len(); got != 1 {
		t.Fatalf("非回传模式下应记录回复，实际 %d 条", got)
	}
	if len(saved) != 1 || saved[0].Content != "收到" {
		t.Fatalf("非回传模式下应落库：%+v", saved)
	}
}

func Test私聊直答仍用最后一条焦点(t *testing.T) {
	cfg := proactiveTestConfig()
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetReply("收到")

	const chatID = "private:u1"
	gc := instance.cacheStore().GetGroupCache(chatID, cfg.MaxCacheMsg)
	gc.Add(cache.Message{MessageID: "m1", GroupID: chatID, UserID: "u1", Content: "在吗"})

	event := onebot.Event{
		Type: "message", DetailType: "private", UserID: "u1", MessageID: "m1",
		Self:    onebot.Self{Platform: "qq", UserID: "bot-openid"},
		Message: []onebot.Segment{onebot.TextSegment("在吗")},
	}
	instance.RouteMessage(context.Background(), "在吗", event, chatID)
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("应只有一次生成调用，实际 %d", len(calls))
	}
	if !strings.Contains(calls[0].SystemPrompt, "只回复最后一条消息") {
		t.Fatalf("私聊应继续使用 focus_last：%s", calls[0].SystemPrompt)
	}
	if strings.Contains(calls[0].SystemPrompt, "（@ 你或叫你昵称）") {
		t.Fatalf("私聊不应使用点名焦点提示：%s", calls[0].SystemPrompt)
	}

	// 私聊回复同样要写回缓存：下一轮才能看到机器人自己说过什么。
	msgs := instance.cacheStore().GetGroupCache(chatID, cfg.MaxCacheMsg).GetAll()
	if len(msgs) != 2 || msgs[1].Content != "收到" {
		t.Fatalf("机器人回复未写入私聊缓存：%+v", msgs)
	}
}

// Test机器人回复写入缓存与状态库 出站回复也要进聊天记录，
// 否则发给大模型的上下文里只有用户的话，模型不知道自己已经答过。
func Test机器人回复写入缓存与状态库(t *testing.T) {
	cfg := proactiveTestConfig()
	instance, fake := newProactiveTestRouter(t, cfg)
	var saved []cache.Message
	instance.SetMessageSink(func(m cache.Message) int64 { saved = append(saved, m); return 0 })
	fake.SetReply("先这样，明天再看。")

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "@bot(bot-openid) 在吗"},
	)
	first := proactiveEvent("u1", "m1", "@bot(bot-openid) 在吗", onebot.MentionSegment("bot-openid"))
	instance.RouteMessage(context.Background(), "@bot(bot-openid) 在吗", first, "g1")
	waitFor(t, "首轮生成调用", func() bool { return len(fake.Calls()) == 1 })
	waitFor(t, "机器人回复写入缓存", func() bool {
		return instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).Len() == 2
	})

	msgs := instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).GetAll()
	if len(msgs) != 2 {
		t.Fatalf("聊天记录应含 1 条用户消息 + 1 条机器人回复，实际 %d 条", len(msgs))
	}
	self := msgs[len(msgs)-1]
	if self.Content != "先这样，明天再看。" {
		t.Fatalf("机器人回复内容不对：%q", self.Content)
	}
	if self.UserID != "bot-openid" || self.Nickname != "bot" {
		t.Fatalf("机器人回复的身份不对：%+v", self)
	}
	if self.MessageID == "" {
		t.Fatal("机器人回复应补一个本地 message_id")
	}
	if len(saved) != 1 || saved[0].Content != "先这样，明天再看。" {
		t.Fatalf("机器人回复未写入 state.db 落库入口：%+v", saved)
	}

	// 下一轮请求的聊天记录里必须能看到机器人上一句。
	seedMessages(instance, cfg,
		cache.Message{MessageID: "m2", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "@bot(bot-openid) 还有呢"},
	)
	second := proactiveEvent("u1", "m2", "@bot(bot-openid) 还有呢", onebot.MentionSegment("bot-openid"))
	instance.RouteMessage(context.Background(), "@bot(bot-openid) 还有呢", second, "g1")
	waitFor(t, "第二轮生成调用", func() bool { return len(fake.Calls()) == 2 })

	calls := fake.Calls()
	if !strings.Contains(calls[1].ChatLog, "先这样，明天再看。") {
		t.Fatalf("第二轮上下文里应包含机器人上一句：%s", calls[1].ChatLog)
	}
}
