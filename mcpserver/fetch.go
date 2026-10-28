package mcpserver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
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

// httpFetcher 默认实现：http(s) 走网络，本地绝对路径从白名单目录读取。
//
// QQ 官方机器人收到的图片在 AstrBot 里是本地临时文件（data/temp/media_image_*.png），
// 不是 http URL，所以必须支持本地路径；但只允许读 llm.image_local_dirs 下的文件，
// 防止模型被诱导去读配置、密钥等任意本地文件。
type httpFetcher struct {
	timeout   time.Duration
	maxBytes  int64
	userAgent string
	localDirs []string
}

// DefaultFetcher 返回默认下载器（tool 超时 30s、单张上限 MaxImageBytes）。
// localDirs 是允许读取的本地图片目录；为空时本地图片一律拒绝。
func DefaultFetcher(localDirs ...string) Fetcher {
	return &httpFetcher{
		timeout:   30 * time.Second,
		maxBytes:  MaxImageBytes,
		userAgent: "good-review-bot/1.0",
		localDirs: normalizeLocalDirs(localDirs),
	}
}

func (f *httpFetcher) FetchImage(ctx context.Context, source string) ([]byte, string, error) {
	source = strings.TrimSpace(source)
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		return f.fetchHTTP(ctx, source)
	}
	return f.fetchLocal(source)
}

func (f *httpFetcher) fetchHTTP(ctx context.Context, url string) ([]byte, string, error) {
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

	data, err := readLimited(resp.Body, f.maxBytes)
	if err != nil {
		return nil, "", err
	}
	return ensureImage(data)
}

func (f *httpFetcher) fetchLocal(source string) ([]byte, string, error) {
	path := source
	if strings.HasPrefix(path, "file://") {
		path = strings.TrimPrefix(path, "file://")
		if unescaped, err := url.PathUnescape(path); err == nil {
			path = unescaped
		}
		// file:///C:/x.png → /C:/x.png
		if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == '/' && path[2] == ':' {
			path = path[1:]
		}
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, "", fmt.Errorf("本地图片必须是绝对路径: %s", truncateStr(source, 80))
	}
	if len(f.localDirs) == 0 {
		return nil, "", fmt.Errorf("未配置 llm.image_local_dirs，拒绝读取本地图片: %s", truncateStr(path, 80))
	}

	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, "", fmt.Errorf("本地图片不存在或无法解析: %s", truncateStr(path, 80))
	}
	if !f.withinLocalDirs(resolved) {
		return nil, "", fmt.Errorf("本地图片不在 llm.image_local_dirs 允许的目录内: %s", truncateStr(path, 80))
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return nil, "", err
	}
	if !info.Mode().IsRegular() {
		return nil, "", fmt.Errorf("本地图片不是普通文件: %s", truncateStr(path, 80))
	}
	if info.Size() > f.maxBytes {
		return nil, "", fmt.Errorf("图片超过 %dMB 上限", f.maxBytes>>20)
	}

	file, err := os.Open(resolved)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	data, err := readLimited(file, f.maxBytes)
	if err != nil {
		return nil, "", err
	}
	return ensureImage(data)
}

// readLimited 读取至多 maxBytes+1 字节；超出上限时返回错误，避免把大文件整个读进内存。
func readLimited(r io.Reader, maxBytes int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("图片超过 %dMB 上限", maxBytes>>20)
	}
	return data, nil
}

// ensureImage 校验非空且 MIME 确实是 image/*。
func ensureImage(data []byte) ([]byte, string, error) {
	if len(data) == 0 {
		return nil, "", fmt.Errorf("图片内容为空")
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		return nil, "", fmt.Errorf("内容不是图片（%s）", mime)
	}
	return data, mime, nil
}

func (f *httpFetcher) withinLocalDirs(path string) bool {
	for _, dir := range f.localDirs {
		root, err := filepath.EvalSymlinks(dir)
		if err != nil {
			continue
		}
		if runtime.GOOS == "windows" {
			// Windows 路径大小写不敏感，Rel 的大小写比较在这里不可靠
			root = strings.ToLower(root)
			path = strings.ToLower(path)
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			continue
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel) {
			return true
		}
	}
	return false
}

// normalizeLocalDirs 展开 ~、转绝对路径、去空项。
func normalizeLocalDirs(dirs []string) []string {
	out := make([]string, 0, len(dirs))
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		if strings.HasPrefix(dir, "~") {
			home, err := os.UserHomeDir()
			if err != nil {
				continue
			}
			dir = filepath.Join(home, strings.TrimLeft(dir[1:], `/\`))
		}
		abs, err := filepath.Abs(dir)
		if err != nil {
			continue
		}
		out = append(out, abs)
	}
	return out
}

// truncateStr 截断用于错误信息的长文本，避免把整条 URL 打进报错。
func truncateStr(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
