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

const testAccount = "qq_official_bot_1906xxxx"

func newClientFor(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewClient(server.URL, nil, "shared-token").For(testAccount)
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

// TestSendPrivateReply私聊参数 私聊走 detail_type=private + user_id，
// 不带 group_id，也不带引用/@ 段（C2C 没有这些语义）。
func TestSendPrivateReply私聊参数(t *testing.T) {
	client := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {
		action, params := decodeAction(t, r)
		if action != "send_message" {
			t.Errorf("action = %q", action)
		}
		if params["detail_type"] != "private" || params["user_id"] != "u-openid" {
			t.Errorf("params = %+v", params)
		}
		if _, ok := params["group_id"]; ok {
			t.Errorf("私聊不应带 group_id：%+v", params)
		}
		segments, ok := params["message"].([]any)
		if !ok || len(segments) != 1 {
			t.Fatalf("message segments = %#v", params["message"])
		}
		if segments[0].(map[string]any)["type"] != "text" {
			t.Errorf("segment = %+v", segments[0])
		}
		writeAction(t, w, map[string]any{"status": "ok", "retcode": 0})
	})

	client.SendPrivateReply("u-openid", "hi")
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

// TestFor完全没有地址时调用失败 锁住"不发到猜出来的地址"：
// 全局和账号覆盖都没配时，动作必须报错而不是静默成功。
func TestFor完全没有地址时调用失败(t *testing.T) {
	root := NewClient("", nil, "")
	other := root.For("qq_official_bot_other")
	if other.Base() != "" {
		t.Fatalf("没配地址时不应有 base，实际 %q", other.Base())
	}
	if _, err := other.GetSelfInfo(); err == nil {
		t.Fatal("未配置地址的账号调用动作必须失败")
	}
}

func TestFor优先账号覆盖其次全局地址(t *testing.T) {
	client := NewClient("http://default:6199/", []Account{
		{ID: "qq_official_bot_a", APIBase: "http://a:6199/"},
	}, "")
	if got := client.For("qq_official_bot_a").Base(); got != "http://a:6199" {
		t.Fatalf("账号覆盖地址 = %q", got)
	}
	if got := client.For("qq_official_bot_b").Base(); got != "http://default:6199" {
		t.Fatalf("未覆盖账号应回退全局地址，实际 %q", got)
	}
	if got := NewClient("", nil, "").For("qq_official_bot_a").Base(); got != "" {
		t.Fatalf("都没配时应返回空地址，实际 %q", got)
	}
}

// TestGetSelfInfoFor带定位参数 多账号共用一个实现端时，昵称查询必须能指明是哪个账号，
// 否则实现端只能按最近事件猜，可能把 A 的名字写给 B。
func TestGetSelfInfoFor带定位参数(t *testing.T) {
	client := newClientFor(t, func(w http.ResponseWriter, r *http.Request) {
		action, params := decodeAction(t, r)
		if action != "get_self_info" {
			t.Errorf("action = %q", action)
		}
		if params["self_id"] != "openid-1" || params["group_id"] != "g1" {
			t.Errorf("params = %+v", params)
		}
		writeAction(t, w, map[string]any{
			"status": "ok", "retcode": 0,
			"data": map[string]any{"user_id": "openid-1", "user_name": "bot-a"},
		})
	})

	info, err := client.GetSelfInfoFor("openid-1", "g1")
	if err != nil {
		t.Fatalf("GetSelfInfoFor: %v", err)
	}
	if info.UserID != "openid-1" || info.UserName != "bot-a" {
		t.Fatalf("info = %+v", info)
	}
}
