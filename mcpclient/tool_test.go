package mcpclient

import (
	"encoding/json"
	"strings"
	"testing"

	"good-review-master/llm"
	"good-review-master/logutil"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// setupTestLogger 初始化 zap 并切到临时目录（logutil 未初始化时任何日志调用都会 nil panic）
func setupTestLogger(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	logutil.SetupLogger()
	t.Cleanup(logutil.Close)
}

// 工具名必须规整成 OpenAI function name 允许的 ^[a-zA-Z0-9_-]{1,64}$，
// 否则模型服务商会直接拒掉整个请求（连带聊天记录一起白送）。
func TestSanitizeToolName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"合法名原样保留", "get_weather", "get_weather"},
		{"带连字符保留", "maps-route", "maps-route"},
		{"点号与斜杠转下划线", "maps.directions/driving", "maps_directions_driving"},
		{"全中文名兜底为 tool", "查询天气", "tool"},
		{"首尾分隔符被裁掉", "__ping__", "ping"},
		{"空串兜底", "", "tool"},
		{"超长截断到 64", strings.Repeat("a", 100), strings.Repeat("a", 64)},
		{"截断后裁掉尾部分隔符", strings.Repeat("a", 70) + "_-", strings.Repeat("a", 64)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeToolName(tc.in); got != tc.want {
				t.Fatalf("sanitizeToolName(%q) = %q，期望 %q", tc.in, got, tc.want)
			}
		})
	}
}

// 工具返回的多段内容要拍平成纯文本；非文本内容转占位说明，不能把二进制塞进上下文。
func TestFlattenContent(t *testing.T) {
	t.Run("多段文本按换行拼接", func(t *testing.T) {
		text, isErr := flattenContent(&mcp.CallToolResult{
			Content: []mcp.Content{
				&mcp.TextContent{Text: "晴"},
				&mcp.TextContent{Text: "32℃"},
			},
		})
		if text != "晴\n32℃" || isErr {
			t.Fatalf("得到 %q, isErr=%v", text, isErr)
		}
	})

	t.Run("服务端标记 isError 要如实上报", func(t *testing.T) {
		text, isErr := flattenContent(&mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: "参数 city 不能为空"}},
			IsError: true,
		})
		if !isErr || text != "参数 city 不能为空" {
			t.Fatalf("得到 %q, isErr=%v", text, isErr)
		}
	})

	t.Run("文本型嵌入资源取内容", func(t *testing.T) {
		text, _ := flattenContent(&mcp.CallToolResult{
			Content: []mcp.Content{&mcp.EmbeddedResource{
				Resource: &mcp.ResourceContents{URI: "file://a.txt", Text: "文件内容"},
			}},
		})
		if text != "文件内容" {
			t.Fatalf("得到 %q", text)
		}
	})

	t.Run("二进制资源只报大小", func(t *testing.T) {
		text, _ := flattenContent(&mcp.CallToolResult{
			Content: []mcp.Content{&mcp.EmbeddedResource{
				Resource: &mcp.ResourceContents{
					URI: "file://a.png", MIMEType: "image/png", Blob: []byte{1, 2, 3, 4},
				},
			}},
		})
		if !strings.Contains(text, "4 字节") || strings.Contains(text, "\x01") {
			t.Fatalf("二进制内容应被省略，得到 %q", text)
		}
	})

	t.Run("非文本内容转占位说明", func(t *testing.T) {
		text, _ := flattenContent(&mcp.CallToolResult{
			Content: []mcp.Content{&mcp.ImageContent{MIMEType: "image/png", Data: []byte("xx")}},
		})
		if !strings.Contains(text, "已省略") {
			t.Fatalf("得到 %q", text)
		}
	})

	t.Run("只有结构化输出时序列化它", func(t *testing.T) {
		text, _ := flattenContent(&mcp.CallToolResult{
			StructuredContent: map[string]any{"temp": 32},
		})
		if text != `{"temp":32}` {
			t.Fatalf("得到 %q", text)
		}
	})

	t.Run("完全空结果给占位文本", func(t *testing.T) {
		text, isErr := flattenContent(&mcp.CallToolResult{})
		if text != "（工具无返回内容）" || isErr {
			t.Fatalf("得到 %q, isErr=%v", text, isErr)
		}
		if nilText, _ := flattenContent(nil); nilText != "" {
			t.Fatalf("nil 结果应返回空串，得到 %q", nilText)
		}
	})
}

// inputSchema 缺失或无法序列化时必须回退成空对象 schema，
// 否则 tools 字段里的 parameters 会变成 null 被模型服务商拒掉。
func TestMarshalInputSchema(t *testing.T) {
	setupTestLogger(t)
	const fallback = `{"type":"object","properties":{}}`

	if got := string(marshalInputSchema(nil)); got != fallback {
		t.Fatalf("nil schema 应回退，得到 %s", got)
	}
	// json.Marshal 对 chan 会报错 → 走回退分支
	if got := string(marshalInputSchema(map[string]any{"bad": make(chan int)})); got != fallback {
		t.Fatalf("不可序列化 schema 应回退，得到 %s", got)
	}

	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"city": map[string]any{"type": "string"}},
	}
	raw := marshalInputSchema(schema)
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("序列化结果不是合法 JSON: %v", err)
	}
	if back["type"] != "object" {
		t.Fatalf("schema 内容丢失: %s", raw)
	}
}

// 环境变量按 key 排序展开，保证同一份配置每次拉起子进程的环境一致
func TestEnvPairs(t *testing.T) {
	if got := envPairs(nil); got != nil {
		t.Fatalf("空 map 应返回 nil，得到 %v", got)
	}
	got := envPairs(map[string]string{"ZEBRA": "1", "API_KEY": "sk-x", "MIDDLE": "2"})
	want := []string{"API_KEY=sk-x", "MIDDLE=2", "ZEBRA=1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("得到 %v，期望 %v", got, want)
	}
}

// 快照内的工具必须按名字典序：map 遍历顺序随机，不排序会让每次重建的
// tools 字段字节不同，直接击穿大模型 prefix cache（命中价变未命中价）。
func TestSortTools(t *testing.T) {
	tools := []llm.Tool{{Name: "zeta"}, {Name: "alpha"}, {Name: "mid"}}
	sortTools(tools)
	got := []string{tools[0].Name, tools[1].Name, tools[2].Name}
	if strings.Join(got, ",") != "alpha,mid,zeta" {
		t.Fatalf("排序结果不对: %v", got)
	}
}
