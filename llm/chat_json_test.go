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
