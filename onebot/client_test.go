package onebot

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"good-review-master/logutil"
)

func TestMain(m *testing.M) {
	// 切到临时目录再初始化日志：logutil 会把日志写到 cwd/log，
	// 不切的话会在包目录里留下一个未被 .gitignore 覆盖的 log/。
	dir, err := os.MkdirTemp("", "good-review-onebot-test-")
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

func newClientFor(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient(server.URL, "shared-token")
}

func writeAction(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

func TestGetLoginInfo(t *testing.T) {
	client := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/get_login_info" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer shared-token" {
			t.Errorf("Authorization = %q", got)
		}
		writeAction(t, w, map[string]any{
			"status": "ok", "retcode": 0,
			"data": map[string]any{"user_id": 123456, "nickname": "bot"},
		})
	})

	info, err := client.GetLoginInfo()
	if err != nil {
		t.Fatalf("GetLoginInfo: %v", err)
	}
	if info.UserID != 123456 || info.Nickname != "bot" {
		t.Fatalf("info = %+v", info)
	}
}

func TestGetLoginInfoRetcodeError(t *testing.T) {
	client := newClientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		// 桥接层未连上 aiocqhttp 时的真实形态：HTTP 200 + retcode 1404。
		writeAction(t, w, map[string]any{
			"status": "failed", "retcode": 1404, "message": "aiocqhttp 适配器未连接",
		})
	})

	if _, err := client.GetLoginInfo(); err == nil {
		t.Fatal("GetLoginInfo must fail on retcode=1404")
	}
}

func TestFetchGroupMsgHistory(t *testing.T) {
	client := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body["group_id"] != "10001" || body["count"] != float64(30) {
			t.Errorf("request body = %+v", body)
		}
		writeAction(t, w, map[string]any{
			"status": "ok", "retcode": 0,
			"data": map[string]any{"messages": []map[string]any{
				{"message_id": 1, "group_id": 10001, "user_id": 2, "raw_message": "hi", "time": 1700000000},
			}},
		})
	})

	msgs, err := client.FetchGroupMsgHistory("10001", 30)
	if err != nil {
		t.Fatalf("FetchGroupMsgHistory: %v", err)
	}
	if len(msgs) != 1 || msgs[0].MessageID != 1 || msgs[0].RawMessage != "hi" {
		t.Fatalf("msgs = %+v", msgs)
	}
}

func TestGetGroupInfoToleratesOpenID(t *testing.T) {
	client := newClientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		writeAction(t, w, map[string]any{
			"status": "ok", "retcode": 0,
			"data": map[string]any{"group_id": "8A2B-openid", "group_name": "测试群"},
		})
	})

	info, err := client.GetGroupInfo("8A2B-openid")
	if err != nil {
		t.Fatalf("GetGroupInfo: %v", err)
	}
	if info.GroupName != "测试群" {
		t.Fatalf("info = %+v", info)
	}
}

func TestSendGroupMessageLogsButDoesNotPanic(t *testing.T) {
	client := newClientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		writeAction(t, w, map[string]any{"status": "ok", "retcode": 0})
	})
	client.SendGroupMessage("10001", "hello")
}

func TestSendGroupReplySendsSegments(t *testing.T) {
	client := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send_group_msg" {
			t.Errorf("path = %s", r.URL.Path)
		}
		var body struct {
			GroupID string    `json:"group_id"`
			Message []Segment `json:"message"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if body.GroupID != "10001" || len(body.Message) != 3 {
			t.Fatalf("body = %+v", body)
		}
		if body.Message[0].Type != "reply" || body.Message[0].Data["id"] != "42" {
			t.Errorf("reply segment = %+v", body.Message[0])
		}
		if body.Message[1].Type != "at" || body.Message[1].Data["qq"] != "42" {
			t.Errorf("at segment = %+v", body.Message[1])
		}
		if body.Message[2].Type != "text" || body.Message[2].Data["text"] != " hi" {
			t.Errorf("text segment = %+v", body.Message[2])
		}
		writeAction(t, w, map[string]any{"status": "ok", "retcode": 0})
	})

	client.SendGroupReply("10001", "42", "42", "hi")
}
