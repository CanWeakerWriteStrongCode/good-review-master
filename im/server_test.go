package im

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"good-review-master/logutil"
	"good-review-master/onebot"
)

// TestMain 初始化 zap：Handler 里有 Info 日志，未初始化会直接 panic。
func TestMain(m *testing.M) {
	// 切到临时目录：logutil 把日志写到 cwd/log，避免在包目录留下未被忽略的产物。
	dir, err := os.MkdirTemp("", "good-review-im-test-")
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

type fakeConsumer struct {
	account   string
	events    []onebot.Event
	err       error
	rejectAll bool
}

func (f *fakeConsumer) AcceptEvent(_ context.Context, account string, event onebot.Event) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	if f.rejectAll {
		return false, nil
	}
	f.account = account
	f.events = append(f.events, event)
	return true, nil
}

func newTestServer(t *testing.T, token string, consumer EventConsumer) *Server {
	t.Helper()
	return NewServer("127.0.0.1:0", token, consumer)
}

func postEvent(t *testing.T, server *Server, account, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/im/event/"+account, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OneBot-Version", "12")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

const groupMessage = `{
	"id":"event-1","time":1700000000.5,"type":"message","detail_type":"group","sub_type":"normal",
	"self":{"platform":"qq","user_id":"OPENID_A"},
	"message_id":"99","alt_message":"@bot 锐评下",
	"message":[
		{"type":"mention","data":{"user_id":"OPENID_A"}},
		{"type":"text","data":{"text":"锐评下"}}
	],
	"group_id":"123456","user_id":"42"
}`

func TestServerAcceptsGroupMessage(t *testing.T) {
	consumer := &fakeConsumer{}
	server := newTestServer(t, "secret", consumer)

	rec := postEvent(t, server, "1906xxxx", "secret", groupMessage)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body)
	}
	if len(consumer.events) != 1 {
		t.Fatalf("consumer got %d events, want 1", len(consumer.events))
	}
	if consumer.account != "1906xxxx" {
		t.Fatalf("account = %q（必须来自路径）", consumer.account)
	}
	got := consumer.events[0]
	if got.GroupID != "123456" || got.UserID != "42" || got.MessageID != "99" {
		t.Fatalf("unexpected event: %+v", got)
	}
	if got.Self.Platform != "qq" || got.Self.UserID != "OPENID_A" {
		t.Fatalf("self 未解码：%+v", got.Self)
	}
	if got.Account != "1906xxxx" {
		t.Fatalf("event.Account 应由路径写入：%q", got.Account)
	}
	if !got.MentionsBot() {
		t.Fatalf("mention 段应与 self.user_id 匹配：%+v", got.Message)
	}
}

func TestServerRejectsBadToken(t *testing.T) {
	consumer := &fakeConsumer{}
	server := newTestServer(t, "secret", consumer)

	rec := postEvent(t, server, "1906xxxx", "wrong", groupMessage)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(consumer.events) != 0 {
		t.Fatalf("consumer must not receive unauthorized event")
	}
}

func TestServerAllowsEmptyToken(t *testing.T) {
	consumer := &fakeConsumer{}
	server := newTestServer(t, "", consumer)

	rec := postEvent(t, server, "1906xxxx", "", groupMessage)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
}

func TestServerRejectsInvalidAccount(t *testing.T) {
	consumer := &fakeConsumer{}
	server := newTestServer(t, "", consumer)

	rec := postEvent(t, server, "bad%20account", "", groupMessage)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestServerRejectsWrongVersion(t *testing.T) {
	consumer := &fakeConsumer{}
	server := newTestServer(t, "", consumer)

	req := httptest.NewRequest(http.MethodPost, "/im/event/1906xxxx", strings.NewReader(groupMessage))
	req.Header.Set("X-OneBot-Version", "11")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestServerAcceptsPrivateMessage(t *testing.T) {
	consumer := &fakeConsumer{}
	server := newTestServer(t, "", consumer)

	rec := postEvent(t, server, "1906xxxx", "", `{
		"id":"event-2","time":1700000001,"type":"message","detail_type":"private","sub_type":"friend",
		"self":{"platform":"qq","user_id":"unknown_selfid"},
		"message_id":"100","alt_message":"你好","message":[{"type":"text","data":{"text":"你好"}}],
		"user_id":"USER_OPENID"
	}`)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body=%s", rec.Code, rec.Body)
	}
	if len(consumer.events) != 1 {
		t.Fatalf("consumer got %d events, want 1", len(consumer.events))
	}
	got := consumer.events[0]
	if !got.IsPrivate() || got.ChatID() != "private:USER_OPENID" {
		t.Fatalf("私聊会话键不对：%+v", got)
	}
}

func TestServerIgnoresNonMessageEvents(t *testing.T) {
	consumer := &fakeConsumer{}
	server := newTestServer(t, "", consumer)

	cases := []string{
		`{"type":"notice","detail_type":"group","self":{"platform":"qq"}}`,
		`{"type":"message","detail_type":"private","self":{"platform":"qq"}}`,
		`{"type":"message","detail_type":"channel","self":{"platform":"qq"}}`,
		`{"type":"message","detail_type":"group","group_id":"1","user_id":"1"}`,
		`{"type":"message","detail_type":"group","self":{"platform":"qq"},"user_id":"1"}`,
		`{"type":"message","detail_type":"group","self":{"platform":"qq"},"group_id":"1"}`,
	}
	for _, body := range cases {
		rec := postEvent(t, server, "1906xxxx", "", body)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204 for %s", rec.Code, body)
		}
	}
	if len(consumer.events) != 0 {
		t.Fatalf("consumer got %d events, want 0", len(consumer.events))
	}
}

func TestServerNotTakenOver返回404(t *testing.T) {
	consumer := &fakeConsumer{rejectAll: true}
	server := newTestServer(t, "", consumer)
	rec := postEvent(t, server, "1906xxxx", "", groupMessage)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404（实现端据此交回自己的 Agent）", rec.Code)
	}
}

func TestServerQueueFull返回503(t *testing.T) {
	consumer := &fakeConsumer{err: errors.New("queue full")}
	server := newTestServer(t, "", consumer)
	rec := postEvent(t, server, "1906xxxx", "", groupMessage)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// TestReadableEventJSON不转义HTML 日志里的 @ 标记要能直接读：
// <@openid> 不能变成 \u003c@openid\u003e。
func TestReadableEventJSON不转义HTML(t *testing.T) {
	raw := []byte(`{"alt_message":"\u003c@openid\u003e 你好","message":[{"type":"text","data":{"text":"\u003c@openid\u003e 你好"}}]}`)
	out := string(readableEventJSON(raw))
	if !strings.Contains(out, `<@openid> 你好`) {
		t.Fatalf("应还原成可读的 @ 标记：%s", out)
	}
	if strings.Contains(out, `\u003c`) || strings.Contains(out, `\u003e`) {
		t.Fatalf("不应再输出 HTML 转义：%s", out)
	}
}
