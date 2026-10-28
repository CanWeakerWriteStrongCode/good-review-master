package llm

import (
	"strings"
	"testing"

	"github.com/sashabaranov/go-openai"
)

// TestMarshalRequestForLogTools在messages前 锁住日志字段顺序：
// tools 必须出现在 messages 之前，且只能出现一次（不能和 go-openai 的同名字段重复）。
func TestMarshalRequestForLogTools在messages前(t *testing.T) {
	req := openai.ChatCompletionRequest{
		Model: "m",
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleSystem, Content: "sys"},
			{Role: openai.ChatMessageRoleUser, Content: "hi"},
		},
		Tools: []openai.Tool{{
			Type:     openai.ToolTypeFunction,
			Function: &openai.FunctionDefinition{Name: "search"},
		}},
	}

	raw, err := marshalRequestForLog(req)
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	body := string(raw)

	// 注意：messages 里可能有 tool 相关文本，这里按字段键定位（带引号 + 冒号）
	toolsAt := strings.Index(body, `"tools":`)
	messagesAt := strings.Index(body, `"messages":`)
	if toolsAt < 0 || messagesAt < 0 {
		t.Fatalf("请求里应同时包含 tools 与 messages：%s", body)
	}
	if toolsAt > messagesAt {
		t.Fatalf("tools 应排在 messages 之前：%s", body)
	}
	if got := strings.Count(body, `"tools":`); got != 1 {
		t.Fatalf("tools 键应只出现一次，实际 %d 次：%s", got, body)
	}
}

func TestMarshalRequestForLog无工具时省略(t *testing.T) {
	raw, err := marshalRequestForLog(openai.ChatCompletionRequest{
		Model:    "m",
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if strings.Contains(string(raw), `"tools"`) {
		t.Fatalf("没有工具时不应输出 tools 字段：%s", raw)
	}
}
