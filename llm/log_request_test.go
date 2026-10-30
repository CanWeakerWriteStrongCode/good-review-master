package llm

import (
	"encoding/json"
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

// TestMarshalRequestForLog不转义HTML 发给模型的请求日志要能直接读 @ 标记。
func TestMarshalRequestForLog不转义HTML(t *testing.T) {
	raw, err := marshalRequestForLog(openai.ChatCompletionRequest{
		Model: "m",
		Messages: []openai.ChatCompletionMessage{{
			Role:    openai.ChatMessageRoleUser,
			Content: `<@openid> 你好`,
		}},
	})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `<@openid>`) {
		t.Fatalf("应保留可读的 @ 标记：%s", body)
	}
	if strings.Contains(body, `\u003c`) || strings.Contains(body, `\u003e`) {
		t.Fatalf("不应输出 HTML 转义：%s", body)
	}
}

// TestMarshalForLog字面反斜杠不破坏JSON 还原转义时不能误伤消息正文里真正的
// "\u003c" 文本：那种情况 JSON 是 `\\u003c`，盲替换会得到非法的 `\<`，
// 日志字段（json.RawMessage）随即序列化失败，只剩一条 bodyError。
func TestMarshalForLog字面反斜杠不破坏JSON(t *testing.T) {
	content := `字面量 \u003c 与 <tag> 和 \u0026，还有 \< 结尾`
	raw, err := marshalRequestForLog(openai.ChatCompletionRequest{
		Model:    "m",
		Messages: []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleUser, Content: content}},
	})
	if err != nil {
		t.Fatalf("序列化失败：%v", err)
	}
	if !json.Valid(raw) {
		t.Fatalf("日志 JSON 已被改坏：%s", raw)
	}
	var decoded struct {
		Messages []openai.ChatCompletionMessage `json:"messages"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("日志 JSON 无法解析：%v\n%s", err, raw)
	}
	if len(decoded.Messages) != 1 || decoded.Messages[0].Content != content {
		t.Fatalf("正文应原样保留，实际 %q", decoded.Messages[0].Content)
	}
	if !strings.Contains(string(raw), `<tag>`) {
		t.Fatalf("真正的 < 仍应还原成可读字符：%s", raw)
	}
}

// TestCountImageContents 用来判断是否跳过完整请求 JSON 日志：
// 只要请求里有内联图片（base64 data URL），就不该把整段正文写进日志。
func TestCountImageContents(t *testing.T) {
	msgs := []Message{
		{Role: RoleSystem, Content: "s"},
		{Role: RoleUser, Content: "u", ImageDataURLs: []string{"data:image/png;base64,AAA"}},
		{Role: RoleUser, Content: "u2", ImageDataURLs: []string{"data:image/png;base64,BBB", "data:image/png;base64,CCC"}},
	}
	if got := countImageContents(msgs); got != 3 {
		t.Fatalf("图片数 = %d，期望 3", got)
	}
	if got := countImageContents(nil); got != 0 {
		t.Fatalf("无消息应为 0，实际 %d", got)
	}
}
