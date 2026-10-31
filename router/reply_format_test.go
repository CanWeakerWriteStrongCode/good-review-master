package router

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"good-review-master/cache"
	"good-review-master/onebot"
)

func TestSentenceReplySchema上限(t *testing.T) {
	var schema struct {
		Properties struct {
			Sentences struct {
				MaxItems int `json:"maxItems"`
				MinItems int `json:"minItems"`
			} `json:"sentences"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(sentenceReplySchema(5), &schema); err != nil {
		t.Fatalf("schema 不是合法 JSON：%v", err)
	}
	if schema.Properties.Sentences.MaxItems != 5 || schema.Properties.Sentences.MinItems != 1 {
		t.Fatalf("句数约束不对：%+v", schema.Properties.Sentences)
	}
}

func TestParseSentenceReply(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    []string
		wantErr bool
	}{
		{name: "纯净JSON", raw: `{"sentences":["一","二"]}`, want: []string{"一", "二"}},
		{name: "代码围栏", raw: "```json\n{\"sentences\":[\"一\"]}\n```", want: []string{"一"}},
		{name: "前后有散文", raw: `结果是：{"sentences":["一","二"]} 完毕`, want: []string{"一", "二"}},
		{name: "空条目过滤", raw: `{"sentences":["一",""," 二 "]}`, want: []string{"一", "二"}},
		{name: "句内换行压平", raw: `{"sentences":["上\n下"]}`, want: []string{"上 下"}},
		{name: "空数组", raw: `{"sentences":[]}`, wantErr: true},
		{name: "非JSON", raw: "好啊", wantErr: true},
		{name: "缺字段", raw: `{"other":1}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSentenceReply(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望失败，实际 %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("解析失败：%v", err)
			}
			if strings.Join(got, "|") != strings.Join(tc.want, "|") {
				t.Fatalf("解析结果 = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// Test回复排版写成短句 最终稿会再发一次排版请求：schema 带上限，回复按句拆行后再发送。
func Test回复排版写成短句(t *testing.T) {
	cfg := proactiveTestConfig()
	cfg.LLMConfig.ReplyMaxSentences = 5
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetReply(strings.Repeat("这是一段很长的原稿，包含很多细节，需要被拆开来说。", 5))
	fake.SetStructuredReply(`{"sentences":["先说结论","再补一句","最后提醒"]}`)

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "@bot(bot-openid) 说两句"},
	)
	event := proactiveEvent("u1", "m1", "@bot(bot-openid) 说两句", onebot.MentionSegment("bot-openid"))
	instance.RouteMessage(context.Background(), "@bot(bot-openid) 说两句", event, "g1")

	waitFor(t, "排版调用", func() bool { return len(fake.StructuredCalls()) == 1 })
	waitFor(t, "机器人回复入缓存", func() bool {
		return instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).Len() == 2
	})
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}

	structured := fake.StructuredCalls()[0]
	if !strings.Contains(string(structured.Schema), `"maxItems": 5`) {
		t.Fatalf("排版 schema 应带上限：%s", structured.Schema)
	}
	userMsg := structured.Messages[len(structured.Messages)-1].Content
	if !strings.Contains(userMsg, "最多 5 句") || !strings.Contains(userMsg, "这是一段很长的原稿") {
		t.Fatalf("排版请求应带上限与原稿：%s", userMsg)
	}

	msgs := instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).GetAll()
	if got := msgs[len(msgs)-1].Content; got != "先说结论\n再补一句\n最后提醒" {
		t.Fatalf("最终发送的回复应逐行分句，实际 %q", got)
	}
}

// TestRewriteReply短回复不排版 不足 llm.reply_rewrite_min_runes 字直接发原文，
// 不产生排版请求；刚好达到下限仍排版。
func TestRewriteReply短回复不排版(t *testing.T) {
	cfg := proactiveTestConfig()
	cfg.LLMConfig.ReplyMaxSentences = 5
	cfg.LLMConfig.ReplyRewriteMinRunes = 60
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetStructuredReply(`{"sentences":["拆过的"]}`)

	minRunes := cfg.LLMConfig.ReplyRewriteMinRunes
	short := strings.Repeat("短", minRunes-1)
	got := instance.rewriteReply(context.Background(), cfg, short, "", "bot", "bot-openid")
	if len(got) != 1 || got[0] != short {
		t.Fatalf("短回复应原样返回，实际 %v", got)
	}
	if calls := len(fake.StructuredCalls()); calls != 0 {
		t.Fatalf("短回复不应排版，实际调用 %d 次", calls)
	}

	boundary := strings.Repeat("长", minRunes)
	if got := instance.rewriteReply(context.Background(), cfg, boundary, "", "bot", "bot-openid"); len(got) != 1 || got[0] != "拆过的" {
		t.Fatalf("达到字数下限应排版，实际 %v", got)
	}
	if calls := len(fake.StructuredCalls()); calls != 1 {
		t.Fatalf("达到字数下限应调用一次排版，实际 %d 次", calls)
	}
}

// TestRewriteReply超出上限合并尾部 模型不守 schema 多给了句子：内容不能丢，
// 多出来的句子并进最后一句，最终仍不超过上限。
func TestRewriteReply超出上限合并尾部(t *testing.T) {
	cfg := proactiveTestConfig()
	cfg.LLMConfig.ReplyMaxSentences = 3
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetStructuredReply(`{"sentences":["一。","二。","三。","四。","五。"]}`)

	got := instance.rewriteReply(context.Background(), cfg, strings.Repeat("原稿", cfg.LLMConfig.ReplyRewriteMinRunes), "", "bot", "bot-openid")
	want := []string{"一。", "二。", "三。四。五。"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("超出的句子应合并进最后一句，实际 %v", got)
	}
}

// Test回复排版失败回退原文 排版结果不可用时必须发原文，绝不能丢回复。
func Test回复排版失败回退原文(t *testing.T) {
	cfg := proactiveTestConfig()
	cfg.LLMConfig.ReplyMaxSentences = 5
	instance, fake := newProactiveTestRouter(t, cfg)
	draft := strings.Repeat("原稿保留。", 30)
	fake.SetReply(draft)
	fake.SetStructuredReply("这不是 JSON")

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "@bot(bot-openid) 在吗"},
	)
	event := proactiveEvent("u1", "m1", "@bot(bot-openid) 在吗", onebot.MentionSegment("bot-openid"))
	instance.RouteMessage(context.Background(), "@bot(bot-openid) 在吗", event, "g1")

	waitFor(t, "排版调用", func() bool { return len(fake.StructuredCalls()) == 1 })
	waitFor(t, "机器人回复入缓存", func() bool {
		return instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).Len() == 2
	})
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}

	msgs := instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).GetAll()
	if got := msgs[len(msgs)-1].Content; got != draft {
		t.Fatalf("排版失败应回退原文，实际 %q", got)
	}
}

// Test回复排版关闭时不调用 reply_max_sentences=0 时直接发原文，不产生排版请求。
func Test回复排版关闭时不调用(t *testing.T) {
	cfg := proactiveTestConfig()
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetReply("直接发原文")
	fake.SetStructuredReply(`{"sentences":["不该用到"]}`)

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "@bot(bot-openid) 在吗"},
	)
	event := proactiveEvent("u1", "m1", "@bot(bot-openid) 在吗", onebot.MentionSegment("bot-openid"))
	instance.RouteMessage(context.Background(), "@bot(bot-openid) 在吗", event, "g1")

	waitFor(t, "机器人回复入缓存", func() bool {
		return instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).Len() == 2
	})
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}
	if got := len(fake.StructuredCalls()); got != 0 {
		t.Fatalf("关闭排版后不应调用，实际 %d 次", got)
	}
	msgs := instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).GetAll()
	if got := msgs[len(msgs)-1].Content; got != "直接发原文" {
		t.Fatalf("应直接发原文，实际 %q", got)
	}
}

// TestJudgeVerdict不再输出背景 裁判只给结论字段：
// 老提示词多给的 background 会被忽略，schema 里也不该再有它。
func TestJudgeVerdict不再输出背景(t *testing.T) {
	verdict, err := parseJudgeVerdict(`{"reply": true, "long_form": false, "focus": "最后一条消息", "background": "旧字段", "reason": "闲聊"}`)
	if err != nil {
		t.Fatalf("多出的旧字段不应导致解析失败：%v", err)
	}
	if !verdict.shouldReply() {
		t.Fatal("reply=true 应被解析出来")
	}
	if strings.Contains(string(proactiveVerdictSchema), `"background"`) {
		t.Fatal("裁判 schema 不应再包含 background")
	}
}

// TestJudgeVerdictLongForm 裁判新增 long_form：解析为 true；老输出缺字段时按 false。
func TestJudgeVerdictLongForm(t *testing.T) {
	withField, err := parseJudgeVerdict(`{"reply": true, "long_form": true, "focus": "最后一条消息", "reason": "查资料"}`)
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	if !withField.LongForm {
		t.Fatal("long_form=true 应被解析出来")
	}
	without, err := parseJudgeVerdict(`{"reply": true, "focus": "最后一条消息", "reason": "闲聊"}`)
	if err != nil {
		t.Fatalf("缺 long_form 不应报错：%v", err)
	}
	if without.LongForm {
		t.Fatal("缺 long_form 时按 false 处理")
	}
	if !strings.Contains(string(proactiveVerdictSchema), "long_form") {
		t.Fatal("裁判 schema 应包含 long_form")
	}
}

// Test直答知识型长篇跳过排版 @ 机器人时裁判说 long_form=true：
// 即使 reply=false 也必须回复，且不强制拆成短句。
func Test直答知识型长篇跳过排版(t *testing.T) {
	cfg := proactiveTestConfig()
	cfg.LLMConfig.ReplyMaxSentences = 5
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetJSONReply(`{"reply": false, "long_form": true, "focus": "最后一条消息", "reason": "知识型"}`)
	fake.SetReply("这是一个需要完整展开的知识型答案，包含背景、原因和结论。")
	fake.SetStructuredReply(`{"sentences":["不该用到"]}`)

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "@bot(bot-openid) 解释一下这个问题"},
	)
	event := proactiveEvent("u1", "m1", "@bot(bot-openid) 解释一下这个问题", onebot.MentionSegment("bot-openid"))
	instance.RouteMessage(context.Background(), "@bot(bot-openid) 解释一下这个问题", event, "g1")

	waitFor(t, "裁判调用", func() bool { return len(fake.JSONCalls()) == 1 })
	waitFor(t, "生成调用", func() bool { return len(fake.Calls()) == 1 })
	waitFor(t, "机器人回复入缓存", func() bool {
		return instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).Len() == 2
	})
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}

	if got := len(fake.StructuredCalls()); got != 0 {
		t.Fatalf("知识型长篇不应排版，实际调用 %d 次", got)
	}
	msgs := instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).GetAll()
	if got := msgs[len(msgs)-1].Content; got != "这是一个需要完整展开的知识型答案，包含背景、原因和结论。" {
		t.Fatalf("长篇应原样发送，实际 %q", got)
	}
}

// Test直答普通问答仍然排版 @ 机器人时 long_form=false：照常拆成短句；
// 排版请求要带上群聊记录、机器人身份和这条待改写的回复。
func Test直答普通问答仍然排版(t *testing.T) {
	cfg := proactiveTestConfig()
	cfg.LLMConfig.ReplyMaxSentences = 5
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetJSONReply(`{"reply": true, "long_form": false, "focus": "最后一条消息", "reason": "闲聊"}`)
	fake.SetReply(strings.Repeat("原稿一句话，先说结论再说原因。", 10))
	fake.SetStructuredReply(`{"sentences":["拆成两句","这是第二句"]}`)

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "@bot(bot-openid) 在吗"},
	)
	event := proactiveEvent("u1", "m1", "@bot(bot-openid) 在吗", onebot.MentionSegment("bot-openid"))
	instance.RouteMessage(context.Background(), "@bot(bot-openid) 在吗", event, "g1")

	waitFor(t, "排版调用", func() bool { return len(fake.StructuredCalls()) == 1 })
	waitFor(t, "机器人回复入缓存", func() bool {
		return instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).Len() == 2
	})
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}

	structured := fake.StructuredCalls()[0]
	userMsg := structured.Messages[len(structured.Messages)-1].Content
	for _, want := range []string{"ID：bot-openid", "【群聊记录】", "@bot(bot-openid) 在吗", "【生成的回复】"} {
		if !strings.Contains(userMsg, want) {
			t.Fatalf("排版请求缺少 %q：%s", want, userMsg)
		}
	}
	msgs := instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).GetAll()
	if got := msgs[len(msgs)-1].Content; got != "拆成两句\n这是第二句" {
		t.Fatalf("普通问答应排版，实际 %q", got)
	}
}

// Test主动接话知识型长篇跳过排版 主动链路直接用裁判给的 long_form。
func Test主动接话知识型长篇跳过排版(t *testing.T) {
	cfg := proactiveTestConfig()
	cfg.LLMConfig.ReplyMaxSentences = 5
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetJSONReply(`{"reply": true, "long_form": true, "focus": "整体上下文", "reason": "需要解释"}`)
	fake.SetReply("一段需要完整展开的解释。")
	fake.SetStructuredReply(`{"sentences":["不该用到"]}`)

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "有人知道这个机制吗"},
	)
	event := proactiveEvent("u1", "m1", "有人知道这个机制吗")
	instance.TryProactive(context.Background(), event, "g1", "有人知道这个机制吗")

	waitFor(t, "生成调用", func() bool { return len(fake.Calls()) == 1 })
	waitFor(t, "机器人回复入缓存", func() bool {
		return instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).Len() == 2
	})
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}
	if got := len(fake.StructuredCalls()); got != 0 {
		t.Fatalf("知识型长篇不应排版，实际 %d 次", got)
	}
	msgs := instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).GetAll()
	if got := msgs[len(msgs)-1].Content; got != "一段需要完整展开的解释。" {
		t.Fatalf("长篇应原样发送，实际 %q", got)
	}
}

// Test主动接话排版带上聊天记录 主动链路的排版请求同样要带群聊记录与待改写回复。
func Test主动接话排版带上聊天记录(t *testing.T) {
	cfg := proactiveTestConfig()
	cfg.LLMConfig.ReplyMaxSentences = 5
	instance, fake := newProactiveTestRouter(t, cfg)
	fake.SetJSONReply(`{"reply": true, "long_form": false, "focus": "整体上下文", "reason": "回答问题"}`)
	fake.SetReply(strings.Repeat("先看日志再定位问题。", 10))
	fake.SetStructuredReply(`{"sentences":["先看日志","再定位问题"]}`)

	seedMessages(instance, cfg,
		cache.Message{MessageID: "m1", GroupID: "g1", UserID: "u1", Nickname: "张三", Content: "有人知道这个报错怎么解吗"},
	)
	event := proactiveEvent("u1", "m1", "有人知道这个报错怎么解吗")
	instance.TryProactive(context.Background(), event, "g1", "有人知道这个报错怎么解吗")

	waitFor(t, "排版调用", func() bool { return len(fake.StructuredCalls()) == 1 })
	waitFor(t, "机器人回复入缓存", func() bool {
		return instance.cacheStore().GetGroupCache("g1", cfg.MaxCacheMsg).Len() == 2
	})
	if err := instance.Wait(); err != nil {
		t.Fatalf("等待异步任务失败：%v", err)
	}
	userMsg := fake.StructuredCalls()[0].Messages[len(fake.StructuredCalls()[0].Messages)-1].Content
	for _, want := range []string{"【群聊记录】", "有人知道这个报错怎么解吗", "【生成的回复】"} {
		if !strings.Contains(userMsg, want) {
			t.Fatalf("主动接话的排版请求缺少 %q：%s", want, userMsg)
		}
	}
}

func TestSplitReplyMessages(t *testing.T) {
	if got := splitReplyMessages(nil, 5); got != nil {
		t.Fatalf("空输入应返回 nil，实际 %v", got)
	}
	three := splitReplyMessages([]string{"一", "二", "三"}, 5)
	if len(three) != 3 || three[0][0] != "一" || three[2][0] != "三" {
		t.Fatalf("3 句应拆成 3 条，实际 %v", three)
	}
	six := splitReplyMessages([]string{"一", "二", "三", "四", "五", "六"}, 5)
	if len(six) != 5 {
		t.Fatalf("6 句最多拆 5 条，实际 %d", len(six))
	}
	if len(six[4]) != 2 || six[4][0] != "五" || six[4][1] != "六" {
		t.Fatalf("多出的句子应合并进最后一条，实际 %v", six[4])
	}
	if got := splitReplyMessages([]string{"一", "二"}, 1); len(got) != 1 || len(got[0]) != 2 {
		t.Fatalf("max<1 应按 1 条处理，实际 %v", got)
	}
}

// TestSendReply分条发送 interval>0 时按句拆成多条：第一条引用+@，后续纯文本。
func TestSendReply分条发送(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	var firstHasReply, firstHasMention, laterHasReply bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Params struct {
				Message []struct {
					Type string         `json:"type"`
					Data map[string]any `json:"data"`
				} `json:"message"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var text strings.Builder
		hasReply, hasMention := false, false
		for _, seg := range body.Params.Message {
			switch seg.Type {
			case "text":
				text.WriteString(seg.Data["text"].(string))
			case "reply":
				hasReply = true
			case "mention":
				hasMention = true
			}
		}
		mu.Lock()
		if len(texts) == 0 {
			firstHasReply, firstHasMention = hasReply, hasMention
		} else {
			laterHasReply = laterHasReply || hasReply
		}
		texts = append(texts, strings.TrimSpace(text.String()))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "retcode": 0, "data": map[string]any{}})
	}))
	defer server.Close()

	cfg := proactiveTestConfig()
	cfg.ReplySendInterval = time.Millisecond
	instance := &Router{
		obClient: onebot.NewClient(server.URL, nil, "").For("qq_test"),
		appCfg:   staticSnapshot(cfg),
	}
	event := onebot.Event{
		Type: "message", DetailType: "group", GroupID: "g1", UserID: "u1", MessageID: "m1",
		Self: onebot.Self{Platform: "qq", UserID: "bot-openid"},
	}
	instance.sendReply(context.Background(), event, "g1", true, []string{"一", "二", "三", "四", "五", "六"}, cfg)

	mu.Lock()
	defer mu.Unlock()
	want := []string{"一", "二", "三", "四", "五\n六"}
	if len(texts) != len(want) {
		t.Fatalf("应发 %d 条，实际 %d 条：%v", len(want), len(texts), texts)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Fatalf("第 %d 条 = %q，期望 %q", i+1, texts[i], want[i])
		}
	}
	if !firstHasReply || !firstHasMention {
		t.Fatalf("第一条应引用并 @ 触发者：reply=%v mention=%v", firstHasReply, firstHasMention)
	}
	if laterHasReply {
		t.Fatal("后续消息不应再引用")
	}
}

// TestSendReply不拆分 interval=0 时保持单条发送。
func TestSendReply不拆分(t *testing.T) {
	var mu sync.Mutex
	var texts []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Params struct {
				Message []struct {
					Type string         `json:"type"`
					Data map[string]any `json:"data"`
				} `json:"message"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		var text strings.Builder
		for _, seg := range body.Params.Message {
			if seg.Type == "text" {
				text.WriteString(seg.Data["text"].(string))
			}
		}
		mu.Lock()
		texts = append(texts, strings.TrimSpace(text.String()))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	cfg := proactiveTestConfig() // ReplySendInterval=0
	instance := &Router{
		obClient: onebot.NewClient(server.URL, nil, "").For("qq_test"),
		appCfg:   staticSnapshot(cfg),
	}
	event := onebot.Event{Type: "message", DetailType: "group", GroupID: "g1", UserID: "u1", MessageID: "m1"}
	instance.sendReply(context.Background(), event, "g1", true, []string{"一", "二", "三"}, cfg)

	mu.Lock()
	defer mu.Unlock()
	if len(texts) != 1 || texts[0] != "一\n二\n三" {
		t.Fatalf("interval=0 应合成一条，实际 %v", texts)
	}
}

// TestSendReply台账幂等 同一组动作重试时只补未发的：已发动作直接跳过。
func TestSendReply台账幂等(t *testing.T) {
	var mu sync.Mutex
	sends := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sends++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "retcode": 0, "data": map[string]any{}})
	}))
	defer server.Close()

	cfg := proactiveTestConfig()
	ledger := NewMemoryLedger()
	instance := &Router{
		workspace: "w1",
		obClient:  onebot.NewClient(server.URL, nil, "").For("w1"),
		appCfg:    staticSnapshot(cfg),
		ledger:    ledger,
	}
	event := onebot.Event{
		Type: "message", DetailType: "group", GroupID: "g1", UserID: "u1", MessageID: "m1",
		Self: onebot.Self{Platform: "qq", UserID: "bot-openid"},
	}
	if err := instance.sendReply(context.Background(), event, "g1", true, []string{"一", "二"}, cfg); err != nil {
		t.Fatalf("首次发送：%v", err)
	}
	if err := instance.sendReply(context.Background(), event, "g1", true, []string{"一", "二"}, cfg); err != nil {
		t.Fatalf("重试：%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sends != 1 {
		t.Fatalf("已发动作不应重发，实际发送 %d 次", sends)
	}
}
