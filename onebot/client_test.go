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

const testAccount = "qq_1906xxxx"

func newClientFor(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient([]Account{{ID: testAccount, APIBase: server.URL}}, "shared-token").For(testAccount)
}

func writeAction(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(body); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}

// decodeAction 读 OneBot 12 动作请求外壳，返回 action 与 params。
func decodeAction(t *testing.T, r *http.Request) (string, map[string]any) {
	t.Helper()
	var body struct {
		Action string         `json:"action"`
		Params map[string]any `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return body.Action, body.Params
}

func TestSendMessage动作外壳(t *testing.T) {
	client := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			t.Errorf("path = %s（OB12 的 HTTP API 统一入口是 /）", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer shared-token" {
			t.Errorf("Authorization = %q", got)
		}
		action, params := decodeAction(t, r)
		if action != "send_message" {
			t.Errorf("action = %q", action)
		}
		if params["detail_type"] != "group" || params["group_id"] != "g1" {
			t.Errorf("params = %+v", params)
		}
		segments, ok := params["message"].([]any)
		if !ok || len(segments) != 3 {
			t.Fatalf("message segments = %#v", params["message"])
		}
		first := segments[0].(map[string]any)
		if first["type"] != "reply" || first["data"].(map[string]any)["message_id"] != "42" {
			t.Errorf("first segment = %+v", first)
		}
		second := segments[1].(map[string]any)
		if second["type"] != "mention" || second["data"].(map[string]any)["user_id"] != "u1" {
			t.Errorf("second segment = %+v", second)
		}
		writeAction(t, w, map[string]any{"status": "ok", "retcode": 0})
	})

	client.SendGroupReply("g1", "42", "u1", "hi")
}

func TestAction失败按retcode判定(t *testing.T) {
	client := newClientFor(t, func(w http.ResponseWriter, _ *http.Request) {
		// HTTP 200 + status=failed 是标准的失败表达，不能只看状态码。
		writeAction(t, w, map[string]any{"status": "failed", "retcode": 1404, "message": "not connected"})
	})

	if _, err := client.GetSelfInfo(); err == nil {
		t.Fatal("status=failed 必须返回错误")
	}
}

func TestGetSelfInfo解析(t *testing.T) {
	client := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {
		action, _ := decodeAction(t, r)
		if action != "get_self_info" {
			t.Errorf("action = %q", action)
		}
		writeAction(t, w, map[string]any{
			"status": "ok", "retcode": 0,
			"data": map[string]any{"user_id": "openid-1", "user_name": "bot-a"},
		})
	})

	info, err := client.GetSelfInfo()
	if err != nil {
		t.Fatalf("GetSelfInfo: %v", err)
	}
	if info.UserID != "openid-1" || info.UserName != "bot-a" {
		t.Fatalf("info = %+v", info)
	}
}

func TestGetGroupMemberInfo参数(t *testing.T) {
	client := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {
		action, params := decodeAction(t, r)
		if action != "get_group_member_info" || params["group_id"] != "g1" || params["user_id"] != "u1" {
			t.Errorf("action=%s params=%+v", action, params)
		}
		writeAction(t, w, map[string]any{
			"status": "ok", "retcode": 0,
			"data": map[string]any{"user_id": "u1", "user_name": "张三", "user_displayname": "老张"},
		})
	})

	info, err := client.GetGroupMemberInfo("g1", "u1")
	if err != nil {
		t.Fatalf("GetGroupMemberInfo: %v", err)
	}
	if info.DisplayName() != "老张" {
		t.Fatalf("DisplayName = %q", info.DisplayName())
	}
}

// TestFor未知账号不带地址 锁住多账号选路：没配 api_base 的账号必须失败，
// 不能悄悄发到别的账号（QQ 官方的 self.user_id 无法用来选账号）。
func TestFor未知账号不带地址(t *testing.T) {
	root := NewClient([]Account{{ID: testAccount, APIBase: "http://127.0.0.1:1"}}, "")
	other := root.For("qq_other")
	if other.Base() != "" {
		t.Fatalf("未配置的账号不应有 base，实际 %q", other.Base())
	}
	if _, err := other.GetSelfInfo(); err == nil {
		t.Fatal("未配置地址的账号调用动作必须失败")
	}
}
