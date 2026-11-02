package assets

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"good-review-master/conversation"
	"good-review-master/internal/testutil"
	"good-review-master/logutil"
	"good-review-master/state"
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

// fakeJobStore 内存版欠条本，带抢占和租约语义。
// 加锁是为了让两个 worker 真并发跑起来时，测得出“同一张图被补两次”这种错。
type fakeJobStore struct {
	mu    sync.Mutex
	jobs  []state.AssetTagJob
	notes *fakeNoteStore
}

func sameJob(a, b state.AssetTagJob) bool {
	return a.WorkspaceID == b.WorkspaceID && a.GroupID == b.GroupID &&
		a.MessageID == b.MessageID && a.AssetIndex == b.AssetIndex
}

// claimable 与 state.Store 里那条 SQL 的 WHERE 条件一致。
func claimable(job state.AssetTagJob, claim state.AssetTagClaim) bool {
	if job.Attempts >= claim.MaxAttempts {
		return false
	}
	if job.Status == "pending" || job.Status == "failed" {
		return true
	}
	// running：只有租约过期（领走它的那一方已经没了）才算可领。
	return job.Status == "running" && job.UpdatedAt < claim.StaleBefore.Unix()
}

func (f *fakeJobStore) ClaimableAssetTagJobs(limit int, claim state.AssetTagClaim) ([]state.AssetTagJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []state.AssetTagJob
	for _, job := range f.jobs {
		if !claimable(job, claim) {
			continue
		}
		out = append(out, job)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeJobStore) ClaimAssetTagJob(job state.AssetTagJob, claim state.AssetTagClaim) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.jobs {
		if !sameJob(f.jobs[i], job) || !claimable(f.jobs[i], claim) {
			continue
		}
		f.jobs[i].Status = "running"
		f.jobs[i].Attempts++
		f.jobs[i].UpdatedAt = claim.Now.Unix()
		return true, nil
	}
	return false, nil
}

func (f *fakeJobStore) UpdateAssetTagJob(job state.AssetTagJob) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.jobs {
		if sameJob(f.jobs[i], job) {
			f.jobs[i] = job
			return nil
		}
	}
	return os.ErrNotExist
}

// job 读当前状态（断言用，不直接摸未加锁的切片）。
func (f *fakeJobStore) job() state.AssetTagJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jobs[0]
}

func (f *fakeJobStore) SaveImageNote(note conversation.ImageNote) error {
	return f.notes.SaveImageNote(note)
}

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
	cfg := &BackfillConfig{Enabled: true, Interval: time.Second, Batch: 10, MaxAttempts: 3, ClaimTimeout: time.Minute}
	worker := NewBackfiller(store, tagger, func() *BackfillConfig { return cfg })
	worker.tick(context.Background(), cfg)

	if got := store.job(); got.Status != "done" || got.Attempts != 1 || got.LastError != "" {
		t.Fatalf("欠条应标记完成：%+v", got)
	}
	if len(notes.notes) != 1 {
		t.Fatalf("描述应写入全局图库：%+v", notes.notes)
	}
}

func TestBackfiller原始地址取不到记终态(t *testing.T) {
	fake := testutil.NewFakeLLM()
	notes := newFakeNoteStore()
	tagger := testTagger(t, t.TempDir(), fake, notes)
	gone := filepath.Join(t.TempDir(), "gone.png")

	store := &fakeJobStore{
		notes: notes,
		jobs: []state.AssetTagJob{{
			WorkspaceID: "ws", GroupID: "g1", MessageID: "m1", AssetIndex: 0,
			Kind: "image", URL: gone, Status: "pending",
		}},
	}
	cfg := &BackfillConfig{Enabled: true, Batch: 10, MaxAttempts: 3, ClaimTimeout: time.Minute}
	worker := NewBackfiller(store, tagger, func() *BackfillConfig { return cfg })
	worker.tick(context.Background(), cfg)

	if got := store.job(); got.Status != "missing" || got.Attempts != 1 {
		t.Fatalf("原始地址取不到应记 missing 终态：%+v", got)
	}
	if calls := len(fake.StructuredCalls()); calls != 0 {
		t.Fatalf("终态不应再调模型，实际 %d 次", calls)
	}
}

func TestBackfiller没有原始地址记终态(t *testing.T) {
	fake := testutil.NewFakeLLM()
	notes := newFakeNoteStore()
	tagger := testTagger(t, t.TempDir(), fake, notes)

	store := &fakeJobStore{
		notes: notes,
		jobs: []state.AssetTagJob{{
			WorkspaceID: "ws", GroupID: "g1", MessageID: "m1", AssetIndex: 0,
			Kind: "image", Status: "pending",
		}},
	}
	cfg := &BackfillConfig{Enabled: true, Batch: 10, MaxAttempts: 3, ClaimTimeout: time.Minute}
	worker := NewBackfiller(store, tagger, func() *BackfillConfig { return cfg })
	worker.tick(context.Background(), cfg)

	// 领到就算一次尝试，所以 attempts 是 1 而不是 0。
	if got := store.job(); got.Status != "missing" || got.Attempts != 1 {
		t.Fatalf("没有原始地址应直接记 missing：%+v", got)
	}
}

// TestBackfiller两个副本不重复补同一张图 是这次改造的重点：
// 两个 worker 真并发跑同一张欠条本，模型只能被叫一次、图库只能写一条。
func TestBackfiller两个副本不重复补同一张图(t *testing.T) {
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
	cfg := &BackfillConfig{Enabled: true, Batch: 10, MaxAttempts: 3, ClaimTimeout: time.Minute}

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			NewBackfiller(store, tagger, func() *BackfillConfig { return cfg }).
				tick(context.Background(), cfg)
		}()
	}
	wg.Wait()

	if calls := len(fake.StructuredCalls()); calls != 1 {
		t.Fatalf("同一张欠条只应被描述一次，实际调了 %d 次模型", calls)
	}
	if len(notes.notes) != 1 {
		t.Fatalf("图库只应写入一条：%+v", notes.notes)
	}
	if got := store.job(); got.Status != "done" || got.Attempts != 1 {
		t.Fatalf("欠条应是 done / attempts 1：%+v", got)
	}
}

// TestBackfiller租约没过期不抢别人手里的 防的是反过来出事：
// 明明另一个副本正在补，这边因为租约设得太短又抢一遍。
func TestBackfiller租约没过期不抢别人手里的(t *testing.T) {
	dir := t.TempDir()
	path := writeTestPNG(t, dir, "a.png", 800, 600)
	fake := testutil.NewFakeLLM()
	notes := newFakeNoteStore()
	tagger := testTagger(t, dir, fake, notes)

	store := &fakeJobStore{
		notes: notes,
		jobs: []state.AssetTagJob{{
			WorkspaceID: "ws", GroupID: "g1", MessageID: "m1", AssetIndex: 0,
			Kind: "image", URL: path, Status: "running", Attempts: 1,
			UpdatedAt: time.Now().Unix(), // 刚被另一个副本领走
		}},
	}
	cfg := &BackfillConfig{Enabled: true, Batch: 10, MaxAttempts: 3, ClaimTimeout: 10 * time.Minute}
	NewBackfiller(store, tagger, func() *BackfillConfig { return cfg }).
		tick(context.Background(), cfg)

	if calls := len(fake.StructuredCalls()); calls != 0 {
		t.Fatalf("别人手上的欠条不该抢，实际调了 %d 次模型", calls)
	}
	if got := store.job(); got.Status != "running" {
		t.Fatalf("状态应保持 running：%+v", got)
	}
}
