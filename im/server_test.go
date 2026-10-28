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
	events []onebot.Event
	err    error
}

func (f *fakeConsumer) AcceptEvent(_ context.Context, event onebot.Event) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, event)
	return nil
}

func newTestServer(t *testing.T, token string, consumer EventConsumer) *Server {
	t.Helper()
	return NewServer("127.0.0.1:0", token, consumer)
}

func postEvent(t *testing.T, server *Server, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/im/event", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	return rec
}

func TestServerAcceptsGroupMessage(t *testing.T) {
	consumer := &fakeConsumer{}
	server := newTestServer(t, "secret", consumer)

	rec := postEvent(t, server, "secret", `{
		"post_type":"message","message_type":"group",
		"group_id":"123456","user_id":"42","nickname":"小明","card":"",
		"raw_message":"[CQ:at,qq=123456] 锐评下","message_id":99,
		"time":1700000000,"self_id":"123456","mentioned":true
	}`)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body=%s", rec.Code, http.StatusAccepted, rec.Body)
	}
	if len(consumer.events) != 1 {
		t.Fatalf("consumer got %d events, want 1", len(consumer.events))
	}
	got := consumer.events[0]
	if got.GroupID != "123456" || got.UserID != "42" || got.MessageID != 99 || got.Time != 1700000000 {
		t.Fatalf("unexpected event: %+v", got)
	}
	if got.SelfID != "123456" {
		t.Fatalf("self_id not decoded: %+v", got)
	}
	if !got.Mentioned {
		t.Fatalf("mentioned not decoded: %+v", got)
	}
}

func TestServerRejectsBadToken(t *testing.T) {
	consumer := &fakeConsumer{}
	server := newTestServer(t, "secret", consumer)

	rec := postEvent(t, server, "wrong", `{"post_type":"message","message_type":"group","group_id":"1"}`)
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

	rec := postEvent(t, server, "", `{"post_type":"message","message_type":"group","group_id":"1","message_id":1}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
}

func TestServerIgnoresNonGroupEvents(t *testing.T) {
	consumer := &fakeConsumer{}
	server := newTestServer(t, "", consumer)

	cases := []string{
		`{"post_type":"notice","message_type":"group","group_id":"1"}`,
		`{"post_type":"message","message_type":"private","group_id":""}`,
		`{"post_type":"message","message_type":"group","group_id":""}`,
	}
	for _, body := range cases {
		rec := postEvent(t, server, "", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for %s", rec.Code, body)
		}
	}
	if len(consumer.events) != 0 {
		t.Fatalf("consumer got %d events, want 0", len(consumer.events))
	}
}

func TestServerRejectsInvalidJSON(t *testing.T) {
	server := newTestServer(t, "", &fakeConsumer{})

	rec := postEvent(t, server, "", `{"post_type":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestServerRejectsTrailingData(t *testing.T) {
	server := newTestServer(t, "", &fakeConsumer{})

	rec := postEvent(t, server, "", `{"post_type":"message","message_type":"group","group_id":"1"} {}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestReadableEventJSONRestoresUnicodeAndNumbers(t *testing.T) {
	raw := []byte(`{"raw_message":"@\u4e0d\u662f\u597d\u8bc4\u5927\u5e08 test","message_id":3484799710571075834}`)

	got := string(readableEventJSON(raw))
	if !strings.Contains(got, "不是好评大师") {
		t.Fatalf("Unicode 转义应还原为中文：%s", got)
	}
	// 消息 ID 超过 2^53，走 float64 会被悄悄改写
	if !strings.Contains(got, "3484799710571075834") {
		t.Fatalf("大整数应逐字保留：%s", got)
	}
}

func TestReadableEventJSONFallsBackToRaw(t *testing.T) {
	raw := []byte(`{not json`)
	if got := string(readableEventJSON(raw)); got != string(raw) {
		t.Fatalf("解析失败应退回原文，got %q", got)
	}
}

func TestServerReturns503WhenQueueFull(t *testing.T) {
	consumer := &fakeConsumer{err: errors.New("queue full")}
	server := newTestServer(t, "", consumer)

	rec := postEvent(t, server, "", `{"post_type":"message","message_type":"group","group_id":"1","message_id":1}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}
