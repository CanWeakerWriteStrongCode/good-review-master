package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sort"
	"strings"

	"good-review-master/llm"
	"good-review-master/logutil"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// buildTransport 按配置构造 MCP 传输层。
// 传入的 ctx 决定会话生命周期：SDK 用它跑 JSON-RPC 连接的读写循环，
// ctx 取消即连接关闭（stdio 子进程也随 exec.CommandContext 一并终止）。
func (s *serverConn) buildTransport(ctx context.Context) (mcp.Transport, error) {
	switch s.cfg.Transport {
	case "stdio":
		cmd := exec.CommandContext(ctx, s.cfg.Command, s.cfg.Args...)
		cmd.Env = append(os.Environ(), envPairs(s.cfg.Env)...)
		// stdout 是 JSON-RPC 通道，绝不能占用；stderr 是子进程日志，转进本项目日志
		if pipe, err := cmd.StderrPipe(); err == nil {
			go scanStderr(s.cfg.Name, pipe)
		}
		return &mcp.CommandTransport{Command: cmd}, nil

	case "http":
		return &mcp.StreamableClientTransport{
			Endpoint:   s.cfg.URL,
			HTTPClient: authHTTPClient(s.cfg.Token),
		}, nil

	case "sse":
		// 老式 SSE 传输（2024-11-05 规范），部分存量 MCP 服务只支持这个
		return &mcp.SSEClientTransport{
			Endpoint:   s.cfg.URL,
			HTTPClient: authHTTPClient(s.cfg.Token),
		}, nil
	}
	return nil, fmt.Errorf("不支持的 transport: %q", s.cfg.Transport)
}

// envPairs 把 map 展开成 KEY=VALUE 列表。按 key 排序，保证同一份配置每次拉起子进程的环境一致。
func envPairs(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, k+"="+env[k])
	}
	return pairs
}

// scanStderr 逐行把 MCP 子进程的 stderr 转成本项目的 Debug 日志。
// 直接透传到 os.Stderr 会污染控制台，丢掉又不好排查，故收进日志文件。
func scanStderr(serverName string, r io.ReadCloser) {
	defer r.Close()
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		logutil.Debug("MCP 子进程日志", "server", serverName, "line", line)
	}
}

// authHTTPClient 构造带可选 Bearer Token 的 HTTP 客户端。
// SDK 的 Streamable/SSE 传输没有暴露 header 注入入口，包一层 RoundTripper 最稳。
// 注意不设 Client.Timeout：streamable HTTP 会挂长连 SSE 流，客户端级超时会把它掐断，
// 超时统一交给每次调用的 ctx 控制。
func authHTTPClient(token string) *http.Client {
	base := http.DefaultTransport.(*http.Transport).Clone()
	if strings.TrimSpace(token) == "" {
		return &http.Client{Transport: base}
	}
	return &http.Client{Transport: authRoundTripper{base: base, token: strings.TrimSpace(token)}}
}

// authRoundTripper 给每个请求补 Authorization 头（调用方已显式设置时不覆盖）
type authRoundTripper struct {
	base  http.RoundTripper
	token string
}

func (rt authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("Authorization") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("Authorization", "Bearer "+rt.token)
	}
	return rt.base.RoundTrip(req)
}

// sanitizeToolName 把 MCP 工具名规整成 OpenAI function name 允许的字符集（^[a-zA-Z0-9_-]{1,64}$）。
// 很多 MCP 服务的工具名带点号、斜杠甚至中文，不规整会被模型服务商直接拒掉整个请求。
func sanitizeToolName(raw string) string {
	var b strings.Builder
	b.Grow(len(raw))
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "_-")
	if out == "" {
		return "tool"
	}
	if len(out) > 64 {
		out = strings.TrimRight(out[:64], "_-")
	}
	return out
}

// marshalInputSchema 把 MCP 工具的 inputSchema 序列化成原始 JSON，直接透传给
// OpenAI 的 function.parameters。服务端没给 schema 时补一个空对象 schema，
// 否则 parameters 会变成 null 被模型服务商拒掉。
func marshalInputSchema(schema any) json.RawMessage {
	if schema == nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	raw, err := json.Marshal(schema)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		logutil.Debug("MCP 工具 inputSchema 无法序列化，回退空 schema", "err", err)
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return raw
}

// sortTools 按工具名字典序原地排序。
// 排序是快照稳定性的关键：map 遍历顺序随机，不排序会导致每次重建快照下发的
// tools 字段字节序不同，直接击穿大模型的 prefix cache，把命中价变成未命中价。
func sortTools(tools []llm.Tool) {
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
}

// flattenContent 把 MCP 返回的多段 Content 拍平成纯文本，并回报服务端是否标记了 isError。
// 非文本内容（图片/音频/资源链接）转成一行占位说明，避免把二进制塞进大模型上下文。
func flattenContent(res *mcp.CallToolResult) (string, bool) {
	if res == nil {
		return "", false
	}
	var parts []string
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			if v != nil {
				parts = append(parts, v.Text)
			}
		case *mcp.EmbeddedResource:
			if v != nil {
				parts = append(parts, flattenResource(v.Resource))
			}
		case nil:
			continue
		default:
			parts = append(parts, fmt.Sprintf("[非文本内容（%T）已省略]", c))
		}
	}
	// 只有结构化输出的工具不带 Content，这时把 StructuredContent 序列化出来给模型看
	if len(parts) == 0 && res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, string(raw))
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if text == "" {
		text = "（工具无返回内容）"
	}
	return text, res.IsError
}

// flattenWithImages 同 flattenContent，但额外把 MCP 返回里的 ImageContent 原样带出
// （不转占位文本），供 agent「看图」把图片回传给视觉模型。Text 部分照常拍平。
func flattenWithImages(res *mcp.CallToolResult) (string, bool, []llm.Image) {
	if res == nil {
		return "", false, nil
	}
	var parts []string
	var imgs []llm.Image
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			if v != nil {
				parts = append(parts, v.Text)
			}
		case *mcp.EmbeddedResource:
			if v != nil {
				parts = append(parts, flattenResource(v.Resource))
			}
		case *mcp.ImageContent:
			if v != nil && len(v.Data) > 0 {
				imgs = append(imgs, llm.Image{Data: v.Data, MIMEType: v.MIMEType})
			}
		case nil:
			continue
		default:
			parts = append(parts, fmt.Sprintf("[非文本内容（%T）已省略]", c))
		}
	}
	if len(parts) == 0 && res.StructuredContent != nil {
		if raw, err := json.Marshal(res.StructuredContent); err == nil {
			parts = append(parts, string(raw))
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	if text == "" && len(imgs) == 0 {
		text = "（工具无返回内容）"
	}
	return text, res.IsError, imgs
}

// flattenResource 把嵌入资源转成文本：文本资源直接取内容，二进制资源只报大小
func flattenResource(rc *mcp.ResourceContents) string {
	if rc == nil {
		return "[空资源]"
	}
	if rc.Text != "" {
		return rc.Text
	}
	if len(rc.Blob) > 0 {
		mime := rc.MIMEType
		if mime == "" {
			mime = "application/octet-stream"
		}
		return fmt.Sprintf("[二进制资源 %s，%s，%d 字节，内容已省略]", rc.URI, mime, len(rc.Blob))
	}
	return fmt.Sprintf("[空资源 %s]", rc.URI)
}
