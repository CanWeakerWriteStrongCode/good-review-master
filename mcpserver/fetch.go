package mcpserver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Fetcher 下载图片 URL，返回字节与 MIME（实现可注入，测试用 fake）。
type Fetcher interface {
	FetchImage(ctx context.Context, url string) (data []byte, mime string, err error)
}

// FetcherFunc 函数式适配 Fetcher
type FetcherFunc func(ctx context.Context, url string) ([]byte, string, error)

func (f FetcherFunc) FetchImage(ctx context.Context, url string) ([]byte, string, error) {
	return f(ctx, url)
}

// MaxImageBytes 单张图片下载上限（防内存被打爆）。
const MaxImageBytes = 8 << 20 // 8MB

// httpFetcher 默认实现：带 UA/超时/限流，嗅探图片 MIME。
type httpFetcher struct {
	timeout   time.Duration
	maxBytes  int64
	userAgent string
}

// DefaultFetcher 返回默认下载器（tool 超时 30s、单张上限 MaxImageBytes）。
func DefaultFetcher() Fetcher {
	return &httpFetcher{timeout: 30 * time.Second, maxBytes: MaxImageBytes, userAgent: "good-review-bot/1.0"}
}

func (f *httpFetcher) FetchImage(ctx context.Context, url string) ([]byte, string, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return nil, "", fmt.Errorf("非 http(s) 图片地址: %s", truncateStr(url, 80))
	}

	ctx, cancel := context.WithTimeout(ctx, f.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	if f.userAgent != "" {
		req.Header.Set("User-Agent", f.userAgent)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("图片下载失败: HTTP %d", resp.StatusCode)
	}

	limited := io.LimitReader(resp.Body, f.maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, "", err
	}
	if int64(len(data)) > f.maxBytes {
		return nil, "", fmt.Errorf("图片超过 %dMB 上限", f.maxBytes>>20)
	}
	if len(data) == 0 {
		return nil, "", fmt.Errorf("图片内容为空")
	}

	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", fmt.Errorf("下载内容不是图片（%s）", mime)
	}
	return data, mime, nil
}

// truncateStr 截断用于错误信息的长文本，避免把整条 URL 打进报错。
func truncateStr(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
