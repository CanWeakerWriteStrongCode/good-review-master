package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

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
	if !strings.Contains(prompt, "message_id 是本窗口内的消息序号") || !strings.Contains(prompt, "nickname 是昵称") {
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
