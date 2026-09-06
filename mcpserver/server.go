package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"good-review-master/logutil"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolName 内嵌 MCP 服务提供给 agent 的「看图」工具名。
const ToolName = "view_image"

// toolSchema view_image 的入参 JSON Schema（url 必填）。
const toolSchema = `{"type":"object","properties":{"url":{"type":"string","description":"要查看的图片地址（候选列表里给出的 url）"}},"required":["url"]}`

const toolDescriptionText = "查看群消息里某张图片的内容。当回答需要看清图片（如图片/表情/截图/图中的文字）时，传入候选图片列表里对应的 url 调用本工具。一次看一张即可，避免重复看同一张。"

// Server 进程内嵌 MCP 服务端：只监听 127.0.0.1，供本机 agent（mcpclient）连回调用 view_image。
// token 非空时所有请求要求 Authorization: Bearer <token>（可选鉴权，防止误暴露被白嫖）。
// 【未来外开放口】对外暴露需参数化监听地址/固定端口 + view_image 下载域名白名单（防 SSRF） + 建议反代 TLS；
// 当前定位为 loopback 自用，start 固定绑 127.0.0.1:0。
type Server struct {
	fetcher Fetcher
	token   string
	handler http.Handler

	mu       sync.Mutex
	httpSrv  *http.Server
	baseAddr string
}

// New 构造内嵌 MCP 服务端。fetcher 传 nil 时用默认下载器。
func New(fetcher Fetcher, token string) *Server {
	if fetcher == nil {
		fetcher = DefaultFetcher()
	}
	s := &Server{fetcher: fetcher, token: token}

	server := mcp.NewServer(&mcp.Implementation{Name: "builtin", Version: "0.1.0"}, nil)
	server.AddTool(&mcp.Tool{
		Name:        ToolName,
		Description: toolDescriptionText,
		InputSchema: json.RawMessage(toolSchema),
	}, s.handleViewImage)

	streamable := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	s.handler = streamable
	return s
}

// Handler 返回带可选 Bearer 鉴权的 streamable HTTP handler（挂到 /mcp）。
func (s *Server) Handler() http.Handler {
	return s.bearerWrap(s.handler)
}

// Start 在 127.0.0.1 的随机端口启动 HTTP 服务，返回实际地址（http://127.0.0.1:port/mcp）。
func (s *Server) Start() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.httpSrv = &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	s.baseAddr = "http://" + ln.Addr().String()
	s.mu.Unlock()

	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logutil.Error("内嵌 MCP 服务端退出异常", "err", err)
		}
	}()
	return s.baseAddr + "/mcp", nil
}

// Addr 返回启动后的 /mcp 地址（未启动返回空）。
func (s *Server) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baseAddr + "/mcp"
}

// Close 关闭内嵌 HTTP 服务。
func (s *Server) Close() error {
	s.mu.Lock()
	hs := s.httpSrv
	s.httpSrv = nil
	s.mu.Unlock()
	if hs == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return hs.Shutdown(ctx)
}

// handleViewImage 执行 view_image：下载 url → 返回 ImageContent。
// 失败时返回文本错误给模型看（模型可换图或放弃），不向上抛。
func (s *Server) handleViewImage(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
		return textResult("看图失败：入参不是合法 JSON", true), nil
	}
	url := strings.TrimSpace(args.URL)
	if url == "" {
		return textResult("看图失败：缺少 url，请传候选列表里某张图片的地址", true), nil
	}

	data, mime, err := s.fetcher.FetchImage(ctx, url)
	if err != nil {
		return textResult("看图失败："+err.Error(), true), nil
	}
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.ImageContent{Data: data, MIMEType: mime}},
	}, nil
}

// bearerWrap 可选鉴权：token 为空则不校验；否则要求 Authorization: Bearer <token>。
func (s *Server) bearerWrap(next http.Handler) http.Handler {
	token := strings.TrimSpace(s.token)
	if token == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.EqualFold(r.Header.Get("Authorization"), "Bearer "+token) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","error":{"code":-32001,"message":"unauthorized"},"id":null}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// textResult 构造纯文本（可带 isError）的工具结果。
func textResult(text string, isErr bool) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: text}},
		IsError: isErr,
	}
}
