package assets

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"good-review-master/cache"
	"good-review-master/internal/testutil"
	"good-review-master/logutil"
	"good-review-master/workspace/state"
)

// TestMain 初始化日志并切到临时目录：补标签路径会打日志，未初始化会 nil panic。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "assets-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(dir); err != nil {
		panic(err)
	}
	logutil.SetupLogger()
	code := m.Run()
	logutil.Close()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// fakeJobStore 内存版欠条本，记录每次更新后的状态。
type fakeJobStore struct {
	jobs  []state.AssetTagJob
	notes *fakeNoteStore
}

func (f *fakeJobStore) PendingAssetTagJobs(limit, maxAttempts int) ([]state.AssetTagJob, error) {
	var out []state.AssetTagJob
	for _, job := range f.jobs {
		if (job.Status == "pending" || job.Status == "failed") && job.Attempts < maxAttempts {
			out = append(out, job)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeJobStore) UpdateAssetTagJob(job state.AssetTagJob) error {
	for i := range f.jobs {
		if f.jobs[i].WorkspaceID == job.WorkspaceID && f.jobs[i].GroupID == job.GroupID &&
			f.jobs[i].MessageID == job.MessageID && f.jobs[i].AssetIndex == job.AssetIndex {
			f.jobs[i] = job
			return nil
		}
	}
	return os.ErrNotExist
}

func (f *fakeJobStore) SaveImageNote(note cache.ImageNote) error { return f.notes.SaveImageNote(note) }

func (f *fakeJobStore) CleanupAssetTagJobs(time.Time, time.Duration) (int64, error) { return 0, nil }

func TestBackfiller补标签并标记完成(t *testing.T) {
	dir := t.TempDir()
	path := writeTestPNG(t, dir, "a.png", 800, 600)
	fake := testutil.NewFakeLLM()
	fake.SetStructuredReply(`{"content":"一张测试图片","info_level":"low","has_text":false}`)
	notes := newFakeNoteStore()
	tagger := testTagger(t, dir, fake, notes)

	store := &fakeJobStore{
		notes: notes,
		jobs: []state.AssetTagJob{{
			WorkspaceID: "ws", GroupID: "g1", MessageID: "m1", AssetIndex: 0,
			Kind: "image", URL: path, Status: "pending",
		}},
	}
	cfg := &BackfillConfig{Enabled: true, Interval: time.Second, Batch: 10, MaxAttempts: 3, CacheDir: dir}
	worker := NewBackfiller(store, tagger, func() *BackfillConfig { return cfg })
	worker.tick(context.Background(), cfg)

	if got := store.jobs[0]; got.Status != "done" || got.Attempts != 1 || got.LastError != "" {
		t.Fatalf("欠条应标记完成：%+v", got)
	}
	if len(notes.notes) != 1 {
		t.Fatalf("描述应写入全局图库：%+v", notes.notes)
	}
}

func TestBackfiller本地副本消失记终态(t *testing.T) {
	fake := testutil.NewFakeLLM()
	notes := newFakeNoteStore()
	tagger := testTagger(t, t.TempDir(), fake, notes)
	missing := filepath.Join(t.TempDir(), "gone.png")

	store := &fakeJobStore{
		notes: notes,
		jobs: []state.AssetTagJob{{
			WorkspaceID: "ws", GroupID: "g1", MessageID: "m1", AssetIndex: 0,
			Kind: "image", LocalPath: missing, Status: "pending",
		}},
	}
	cfg := &BackfillConfig{Enabled: true, Batch: 10, MaxAttempts: 3}
	worker := NewBackfiller(store, tagger, func() *BackfillConfig { return cfg })
	worker.tick(context.Background(), cfg)

	if got := store.jobs[0]; got.Status != "missing" {
		t.Fatalf("本地副本和 URL 都没了应记 missing 终态：%+v", got)
	}
	if calls := len(fake.StructuredCalls()); calls != 0 {
		t.Fatalf("终态不应再调模型，实际 %d 次", calls)
	}
}
