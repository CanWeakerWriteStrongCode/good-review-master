package assets

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/internal/testutil"
	"good-review-master/pipeline"
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

func testTagger(t *testing.T, dir string, fake *testutil.FakeLLM, store ImageNoteStore) *ImageTagger {
	t.Helper()
	cfg := &config.Config{LLMTimeout: 2 * time.Second}
	cfg.LLMConfig.ImageNoteMaxSide = 640
	cfg.LLMConfig.ImageNoteMaxPixels = 400000
	cfg.LLMConfig.ImageNoteTimeoutSec = 2
	cfg.LLMConfig.ImageNoteRetries = 2
	cfg.LLMConfig.ImageNoteConcurrency = 2
	cfg.LLMConfig.ImageLocalDirs = []string{dir}
	cfg.LLMConfig.ModelName = "fake"
	cfg.Assets = config.AssetsConf{
		MessageBudget:  5 * time.Second,
		TagConcurrency: 2,
		PromptVersion:  "test",
	}
	snapshot := func() *config.Config { return cfg }
	return NewImageTagger(snapshot, fake, func() string { return "描述图片" }, store)
}

func TestTagger描述并去重(t *testing.T) {
	dir := t.TempDir()
	path := writeTestPNG(t, dir, "a.png", 1200, 2400)
	fake := testutil.NewFakeLLM()
	fake.SetStructuredReply(`{"content":"新闻截图，标题关于台风停运","info_level":"high","has_text":true}`)
	store := newFakeNoteStore()
	tagger := testTagger(t, dir, fake, store)

	result := tagger.TagURLs(context.Background(), []string{path})
	if len(result.ImageIDs) != 1 || result.ImageIDs[0].SHA == "" || result.ImageIDs[0].Size == 0 {
		t.Fatalf("应算出图片内容标识：%+v", result.ImageIDs)
	}
	if len(result.ImageNotes) != 1 || result.ImageNotes[0].Status != "ok" || result.ImageNotes[0].Note == "" {
		t.Fatalf("应写入描述：%+v", result.ImageNotes)
	}
	if result.Status != pipeline.AssetStatusOK {
		t.Fatalf("status = %q", result.Status)
	}
	if store.saves != 1 || len(fake.StructuredCalls()) != 1 {
		t.Fatalf("首次应描述一次并入库：saves=%d calls=%d", store.saves, len(fake.StructuredCalls()))
	}
	if result.ImageNotes[0].PromptVersion != "test" {
		t.Fatalf("prompt_version = %q", result.ImageNotes[0].PromptVersion)
	}

	second := tagger.TagURLs(context.Background(), []string{path})
	if second.ImageIDs[0] != result.ImageIDs[0] || second.ImageNotes[0].Note != result.ImageNotes[0].Note {
		t.Fatalf("同图应命中同一条描述：%+v", second.ImageNotes)
	}
	if len(fake.StructuredCalls()) != 1 || store.saves != 1 {
		t.Fatalf("命中后不应再描述：calls=%d saves=%d", len(fake.StructuredCalls()), store.saves)
	}
}

func TestTagger失败重试上限(t *testing.T) {
	dir := t.TempDir()
	path := writeTestPNG(t, dir, "b.png", 400, 400)
	fake := testutil.NewFakeLLM()
	fake.SetStructuredReply("这不是 JSON")
	store := newFakeNoteStore()
	tagger := testTagger(t, dir, fake, store)

	first := tagger.TagURLs(context.Background(), []string{path})
	if first.Status != pipeline.AssetStatusFailed || len(first.Deferred) != 1 {
		t.Fatalf("失败应进入欠条：%+v", first)
	}
	if calls := len(fake.StructuredCalls()); calls != 1 || store.fails != 1 {
		t.Fatalf("第一次失败应记录一次：calls=%d fails=%d", calls, store.fails)
	}

	// attempts=1 < 2：仍会重试一次。
	tagger.TagURLs(context.Background(), []string{path})
	if calls := len(fake.StructuredCalls()); calls != 2 {
		t.Fatalf("未到重试上限应再试一次，实际 calls=%d", calls)
	}

	// attempts=2 达到上限：不再调用。
	result := tagger.TagURLs(context.Background(), []string{path})
	if calls := len(fake.StructuredCalls()); calls != 2 {
		t.Fatalf("达到重试上限后不应再调用，实际 calls=%d", calls)
	}
	if result.Status != pipeline.AssetStatusFailed {
		t.Fatalf("仍应是 failed，实际 %q", result.Status)
	}
}

func TestTagger时间预算耗尽进入欠条(t *testing.T) {
	dir := t.TempDir()
	path := writeTestPNG(t, dir, "c.png", 100, 100)
	fake := testutil.NewFakeLLM()
	fake.SetStructuredReply(`{"content":"图","info_level":"low","has_text":false}`)
	store := newFakeNoteStore()
	cfg := &config.Config{LLMTimeout: time.Second}
	cfg.LLMConfig.ImageNoteMaxSide = 640
	cfg.LLMConfig.ImageNoteTimeoutSec = 2
	cfg.LLMConfig.ImageNoteRetries = 2
	cfg.LLMConfig.ImageLocalDirs = []string{dir}
	cfg.Assets = config.AssetsConf{MessageBudget: time.Minute, TagConcurrency: 1}
	tagger := NewImageTagger(func() *config.Config { return cfg }, fake, func() string { return "p" }, store)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result := tagger.TagURLs(ctx, []string{path})
	if result.Status != pipeline.AssetStatusDeferred {
		t.Fatalf("预算耗尽应为 deferred，实际 %q", result.Status)
	}
	if len(result.Deferred) != 1 || len(result.Deferred[0].Data) == 0 {
		t.Fatalf("欠条应带已经取到的原图：%+v", result.Deferred)
	}
}

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

func TestDeferredBytes保存与过期清理(t *testing.T) {
	dir := t.TempDir()
	path, err := SaveDeferredBytes(dir, cache.ImageID{SHA: "abc", Size: 3}, []byte("img"), 1<<20)
	if err != nil || path == "" {
		t.Fatalf("保存副本失败：%q, %v", path, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("副本不存在：%v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if removed := CleanupDeferredBytes(dir, 5*time.Millisecond, time.Now()); removed != 1 {
		t.Fatalf("应清理 1 个过期副本，实际 %d", removed)
	}
}
