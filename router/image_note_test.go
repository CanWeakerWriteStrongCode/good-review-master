package router

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"good-review-master/cache"
)

// fakeNoteStore 内存版全局图库，记录调用次数供断言。
type fakeNoteStore struct {
	notes map[cache.ImageID]cache.ImageNote
	saves int
	fails int
}

func newFakeNoteStore() *fakeNoteStore {
	return &fakeNoteStore{notes: make(map[cache.ImageID]cache.ImageNote)}
}

func (f *fakeNoteStore) GetImageNotes(ids []cache.ImageID) (map[cache.ImageID]cache.ImageNote, error) {
	out := make(map[cache.ImageID]cache.ImageNote, len(ids))
	for _, id := range ids {
		if note, ok := f.notes[id]; ok {
			out[id] = note
		}
	}
	return out, nil
}

func (f *fakeNoteStore) SaveImageNote(note cache.ImageNote) error {
	f.saves++
	f.notes[note.ImageID] = note
	return nil
}

func (f *fakeNoteStore) MarkImageNoteFailed(note cache.ImageNote) error {
	f.fails++
	f.notes[note.ImageID] = note
	return nil
}

// writeTestPNG 生成一张指定尺寸的 PNG，供降采样/哈希测试使用。
func writeTestPNG(t *testing.T, dir, name string, width, height int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码测试图片失败：%v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatalf("写入测试图片失败：%v", err)
	}
	return path
}

// TestEnsureImageNotes描述并去重 未命中调一次描述写库；第二次同图直接命中，不重复调用。
func TestEnsureImageNotes描述并去重(t *testing.T) {
	dir := t.TempDir()
	path := writeTestPNG(t, dir, "a.png", 1200, 2400)

	cfg := proactiveTestConfig()
	cfg.LLMConfig.ImageNoteMaxSide = 640
	cfg.LLMConfig.ImageNoteMaxPixels = 400000
	cfg.LLMConfig.ImageNoteTimeoutSec = 2
	cfg.LLMConfig.ImageNoteRetries = 2
	cfg.LLMConfig.ImageNoteConcurrency = 2
	cfg.LLMConfig.ImageLocalDirs = []string{dir}
	instance, fake := newProactiveTestRouter(t, cfg)
	store := newFakeNoteStore()
	instance.SetImageNoteStore(store)
	fake.SetStructuredReply(`{"content":"新闻截图，标题关于台风停运","info_level":"high","has_text":true}`)

	ids, notes := instance.EnsureImageNotes(context.Background(), []string{path})
	if len(ids) != 1 || ids[0].SHA == "" || ids[0].Size == 0 {
		t.Fatalf("应算出图片内容标识：%+v", ids)
	}
	if len(notes) != 1 || notes[0].Status != "ok" || notes[0].Note == "" {
		t.Fatalf("应写入描述：%+v", notes)
	}
	if store.saves != 1 || len(fake.StructuredCalls()) != 1 {
		t.Fatalf("首次应描述一次并入库：saves=%d calls=%d", store.saves, len(fake.StructuredCalls()))
	}

	// 同一张图再来一次：命中全局图库，不再调模型。
	ids2, notes2 := instance.EnsureImageNotes(context.Background(), []string{path})
	if ids2[0] != ids[0] || notes2[0].Note != notes[0].Note {
		t.Fatalf("同图应命中同一条描述：%+v", notes2)
	}
	if len(fake.StructuredCalls()) != 1 || store.saves != 1 {
		t.Fatalf("命中后不应再描述：calls=%d saves=%d", len(fake.StructuredCalls()), store.saves)
	}
}

// TestEnsureImageNotes失败重试上限 描述失败按 attempts 记录；达到重试上限后不再调用。
func TestEnsureImageNotes失败重试上限(t *testing.T) {
	dir := t.TempDir()
	path := writeTestPNG(t, dir, "b.png", 400, 400)

	cfg := proactiveTestConfig()
	cfg.LLMConfig.ImageNoteMaxSide = 640
	cfg.LLMConfig.ImageNoteMaxPixels = 400000
	cfg.LLMConfig.ImageNoteTimeoutSec = 2
	cfg.LLMConfig.ImageNoteRetries = 2
	cfg.LLMConfig.ImageNoteConcurrency = 1
	cfg.LLMConfig.ImageLocalDirs = []string{dir}
	instance, fake := newProactiveTestRouter(t, cfg)
	store := newFakeNoteStore()
	instance.SetImageNoteStore(store)
	fake.SetStructuredReply("这不是 JSON")

	if _, notes := instance.EnsureImageNotes(context.Background(), []string{path}); len(notes) != 1 || notes[0].Note != "" {
		t.Fatalf("失败时应返回空描述：%+v", notes)
	}
	if calls := len(fake.StructuredCalls()); calls != 1 || store.fails != 1 {
		t.Fatalf("第一次失败应记录一次：calls=%d fails=%d", calls, store.fails)
	}

	// attempts=1 < 2：仍会重试一次。
	instance.EnsureImageNotes(context.Background(), []string{path})
	if calls := len(fake.StructuredCalls()); calls != 2 {
		t.Fatalf("未到重试上限应再试一次，实际 calls=%d", calls)
	}

	// attempts=2 达到上限：不再调用。
	instance.EnsureImageNotes(context.Background(), []string{path})
	if calls := len(fake.StructuredCalls()); calls != 2 {
		t.Fatalf("达到重试上限后不应再调用，实际 calls=%d", calls)
	}
}

// Test候选列表注入图片描述 有描述的行带"内容/信息量/文字"，没有描述的保持原样。
func Test候选列表注入图片描述(t *testing.T) {
	cfg := proactiveTestConfig()
	instance, _ := newProactiveTestRouter(t, cfg)
	msgs := []cache.Message{{
		MessageID: "m1", Nickname: "张三",
		Images: []string{"u1", "u2"},
		ImageNotes: []cache.ImageNote{
			{Note: "新闻截图，标题关于台风", InfoLevel: "high", HasText: true, Status: "ok"},
			{},
		},
	}}
	block := instance.imageCandidatesBlock(msgs)
	if !strings.Contains(block, "内容:新闻截图，标题关于台风 信息量:high 文字:有") {
		t.Fatalf("有描述的行应带内容/信息量：%s", block)
	}
	lines := strings.Split(block, "\n")
	for _, line := range lines {
		if strings.Contains(line, "url:u2") && strings.Contains(line, "内容:") {
			t.Fatalf("无描述的行不应带内容：%s", line)
		}
	}
}

// TestDownscaleImage 降采样同时受长边与总像素约束，输出可解码的 JPEG。
func TestDownscaleImage(t *testing.T) {
	dir := t.TempDir()
	path := writeTestPNG(t, dir, "big.png", 1200, 2400)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out, err := downscaleImage(data, 640, 400000)
	if err != nil {
		t.Fatalf("downscaleImage: %v", err)
	}
	img, format, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("降采样结果无法解码：%v", err)
	}
	if format != "jpeg" {
		t.Fatalf("应统一编码成 JPEG，实际 %s", format)
	}
	b := img.Bounds()
	if longest := max(b.Dx(), b.Dy()); longest > 640 {
		t.Fatalf("长边应 ≤ 640，实际 %d", longest)
	}
	if b.Dx()*b.Dy() > 400000 {
		t.Fatalf("总像素应 ≤ 40 万，实际 %d", b.Dx()*b.Dy())
	}
	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("JPEG 解码失败：%v", err)
	}
}
