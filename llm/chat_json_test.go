package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// TestChatJSON降级链 锁住 response_format 的能力协商：
// json_schema 被拒后自动换 json_object，且降级结果会被记住（下一次不再先试 schema）。
func TestChatJSON降级链(t *testing.T) {
	var mu sync.Mutex
	var formats []string // 每次请求的 response_format.type（"none" 表示没带）

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ResponseFormat *struct {
				Type string `json:"type"`
			} `json:"response_format"`
		}
		_ = json.Unmarshal(body, &req)
		format := "none"
		if req.ResponseFormat != nil {
			format = req.ResponseFormat.Type
		}

		mu.Lock()
		formats = append(formats, format)
		mu.Unlock()

		if format == "json_schema" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"response_format json_schema is not supported","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"x","object":"chat.completion","created":1,"model":"test",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"reply\": true, \"reason\": \"ok\"}"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.Close()

	client := NewOpenAIAdapter("test-key", server.URL, "test-model", 0.6, 0.95)
	schema := json.RawMessage(`{"type":"object","properties":{"reply":{"type":"boolean"}},"required":["reply"],"additionalProperties":false}`)
	messages := []Message{{Role: RoleUser, Content: "要不要回？"}}

	for i := 0; i < 2; i++ {
		resp, err := client.ChatJSON(context.Background(), messages, schema)
		if err != nil {
			t.Fatalf("第 %d 次调用失败：%v", i+1, err)
		}
		if resp.Content == "" {
			t.Fatalf("第 %d 次调用没有返回内容", i+1)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// 第一次：schema 被拒 → json_object；第二次直接 json_object（降级已记住）
	want := []string{"json_schema", "json_object", "json_object"}
	if len(formats) != len(want) {
		t.Fatalf("请求档位 = %v，期望 %v", formats, want)
	}
	for i := range want {
		if formats[i] != want[i] {
			t.Fatalf("请求档位 = %v，期望 %v", formats, want)
		}
	}
}

// TestChatJSON裁判关闭思考 裁判判定不该带推理模型的思考阶段（省 token、快十倍），
// 而主回复路径（MutiChatWithTool）的请求里不能出现这个字段。
func TestChatJSON裁判关闭思考(t *testing.T) {
	var mu sync.Mutex
	var judgeFlags []bool // 每次 ChatJSON 请求是否带 enable_thinking=false
	var mainHasKwargs bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
			Tools              []any          `json:"tools"`
		}
		_ = json.Unmarshal(body, &req)
		enabled, ok := req.ChatTemplateKwargs["enable_thinking"].(bool)
		noThinking := ok && !enabled

		mu.Lock()
		if r.URL.Path == "/chat/completions" && req.Tools == nil {
			judgeFlags = append(judgeFlags, noThinking)
		} else if req.Tools != nil {
			mainHasKwargs = len(req.ChatTemplateKwargs) > 0
		}
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"x","object":"chat.completion","created":1,"model":"test",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.Close()

	client := NewOpenAIAdapter("test-key", server.URL, "test-model", 0.6, 0.95)
	schema := json.RawMessage(`{"type":"object","properties":{"reply":{"type":"boolean"}},"required":["reply"],"additionalProperties":false}`)

	if _, err := client.ChatJSON(context.Background(), []Message{{Role: RoleUser, Content: "要不要回？"}}, schema); err != nil {
		t.Fatalf("裁判调用失败：%v", err)
	}
	if _, err := client.MutiChatWithTool(context.Background(), []Message{{Role: RoleUser, Content: "聊天"}},
		[]Tool{{Name: "t", ParamsJSON: json.RawMessage(`{"type":"object"}`)}}); err != nil {
		t.Fatalf("主回复调用失败：%v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(judgeFlags) != 1 || !judgeFlags[0] {
		t.Fatalf("裁判请求应带 enable_thinking=false，实际 %v", judgeFlags)
	}
	if mainHasKwargs {
		t.Fatal("主回复路径不应带 chat_template_kwargs")
	}
}

// TestChatJSON后端拒绝chat_template_kwargs时剥离 严格云端会把未知字段当 400：
// 剥离后重试同一档，且剥离结果被记住（后续请求不再带）。
func TestChatJSON后端拒绝chat_template_kwargs时剥离(t *testing.T) {
	var mu sync.Mutex
	var withKwargs []bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			ChatTemplateKwargs map[string]any `json:"chat_template_kwargs"`
		}
		_ = json.Unmarshal(body, &req)

		mu.Lock()
		withKwargs = append(withKwargs, len(req.ChatTemplateKwargs) > 0)
		mu.Unlock()

		if len(req.ChatTemplateKwargs) > 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unrecognized request argument supplied: chat_template_kwargs","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"x","object":"chat.completion","created":1,"model":"test",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	}))
	defer server.Close()

	client := NewOpenAIAdapter("test-key", server.URL, "test-model", 0.6, 0.95)
	schema := json.RawMessage(`{"type":"object","properties":{"reply":{"type":"boolean"}},"required":["reply"],"additionalProperties":false}`)
	for i := 0; i < 2; i++ {
		if _, err := client.ChatJSON(context.Background(), []Message{{Role: RoleUser, Content: "要不要回？"}}, schema); err != nil {
			t.Fatalf("第 %d 次调用失败：%v", i+1, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// 第一次带字段被拒 → 剥离重试成功；第二次直接不带（已记住）
	want := []bool{true, false, false}
	if len(withKwargs) != len(want) {
		t.Fatalf("请求序列 = %v，期望 %v", withKwargs, want)
	}
	for i := range want {
		if withKwargs[i] != want[i] {
			t.Fatalf("请求序列 = %v，期望 %v", withKwargs, want)
		}
	}
}

// TestChatJSON非格式错误不降级 鉴权/网络类错误不是"能力不足"，不能触发降级重试。
func TestChatJSON非格式错误不降级(t *testing.T) {
	var mu sync.Mutex
	requests := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid api key","type":"invalid_request_error"}}`))
	}))
	defer server.Close()

	client := NewOpenAIAdapter("bad-key", server.URL, "test-model", 0.6, 0.95)
	_, err := client.ChatJSON(context.Background(), []Message{{Role: RoleUser, Content: "x"}}, json.RawMessage(`{"type":"object"}`))
	if err == nil {
		t.Fatal("鉴权失败应返回错误")
	}

	mu.Lock()
	defer mu.Unlock()
	if requests != 1 {
		t.Fatalf("非格式错误只应请求一次，实际 %d 次", requests)
	}
}
