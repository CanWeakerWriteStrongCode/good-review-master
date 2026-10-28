package mcpserver

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 最小 PNG 头，够 http.DetectContentType 识别成 image/png。
var pngHeader = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x00}

func writeTestFile(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("写测试文件失败：%v", err)
	}
	return path
}

func TestFetchLocalImageInAllowedDir(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "media_image_1.png", pngHeader)

	data, mime, err := DefaultFetcher(dir).FetchImage(context.Background(), path)
	if err != nil {
		t.Fatalf("允许目录内的图片应可读：%v", err)
	}
	if mime != "image/png" {
		t.Fatalf("mime = %q, want image/png", mime)
	}
	if len(data) != len(pngHeader) {
		t.Fatalf("读到 %d 字节，want %d", len(data), len(pngHeader))
	}
}

func TestFetchLocalImageFileURL(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "media_image_2.png", pngHeader)

	source := "file://" + filepath.ToSlash(path)
	if _, mime, err := DefaultFetcher(dir).FetchImage(context.Background(), source); err != nil {
		t.Fatalf("file:// 地址应可读：%v", err)
	} else if mime != "image/png" {
		t.Fatalf("mime = %q, want image/png", mime)
	}
}

func TestFetchLocalImageOutsideAllowedDir(t *testing.T) {
	allowed := t.TempDir()
	other := t.TempDir()
	path := writeTestFile(t, other, "secret.png", pngHeader)

	_, _, err := DefaultFetcher(allowed).FetchImage(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "允许的目录") {
		t.Fatalf("允许目录外的图片必须拒绝，err = %v", err)
	}
}

func TestFetchLocalImageRejectsNonImage(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "notes.txt", []byte("not an image"))

	_, _, err := DefaultFetcher(dir).FetchImage(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "不是图片") {
		t.Fatalf("非图片内容必须拒绝，err = %v", err)
	}
}

func TestFetchLocalImageWithoutAllowedDirs(t *testing.T) {
	dir := t.TempDir()
	path := writeTestFile(t, dir, "media_image_3.png", pngHeader)

	_, _, err := DefaultFetcher().FetchImage(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "image_local_dirs") {
		t.Fatalf("未配置目录时必须拒绝本地图片，err = %v", err)
	}
}
