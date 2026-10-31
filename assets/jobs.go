package assets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"good-review-master/cache"
	"good-review-master/logutil"
	"good-review-master/workspace/state"
)

// JobStore 是欠条与图库的持久化边界，由 state.Store 实现。
type JobStore interface {
	PendingAssetTagJobs(limit, maxAttempts int) ([]state.AssetTagJob, error)
	UpdateAssetTagJob(job state.AssetTagJob) error
	SaveImageNote(note cache.ImageNote) error
	CleanupAssetTagJobs(now time.Time, retention time.Duration) (int64, error)
}

// TagOne 给单个 URL 打标签（后台补标签用）。成功时返回图库记录。
func (t *ImageTagger) TagOne(ctx context.Context, url string, attempts int) (cache.ImageNote, error) {
	cfg := t.cfg()
	data, mime, err := t.fetcher().FetchImage(ctx, url)
	if err != nil {
		return cache.ImageNote{}, fmt.Errorf("%w: %v", ErrSourceGone, err)
	}
	id := imageID(data)
	if existing, err := t.notes.GetImageNotes([]cache.ImageID{id}); err == nil {
		if note, ok := existing[id]; ok && note.Status == "ok" {
			return note, nil
		}
	}
	return t.describeImage(ctx, cfg, data, mime, id, attempts)
}

// ErrSourceGone 表示原始地址已经取不到（本地临时文件被清掉、链接过期）。
// 重试没意义，后台直接把欠条记成 missing 终态。
var ErrSourceGone = errors.New("原始地址取不到")

// Backfiller 常驻后台补标签：定期扫描 pending/failed 欠条。
type Backfiller struct {
	store  JobStore
	tagger *ImageTagger
	cfg    func() *BackfillConfig
}

// BackfillConfig 只暴露后台需要的几个参数，便于测试。
type BackfillConfig struct {
	Enabled     bool
	Interval    time.Duration
	Batch       int
	MaxAttempts int
	Retention   time.Duration
}

// NewBackfiller 创建补标签 worker。
func NewBackfiller(store JobStore, tagger *ImageTagger, cfg func() *BackfillConfig) *Backfiller {
	return &Backfiller{store: store, tagger: tagger, cfg: cfg}
}

// Run 常驻循环。关闭由 ctx 控制。
func (b *Backfiller) Run(ctx context.Context) {
	if b == nil || b.store == nil || b.tagger == nil || b.cfg == nil {
		return
	}
	for {
		cfg := b.cfg()
		interval := cfg.Interval
		if !cfg.Enabled || interval <= 0 {
			// 关闭时低频检查设置是否被重新打开。
			interval = 30 * time.Second
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if !cfg.Enabled || cfg.Interval <= 0 {
			continue
		}
		b.tick(ctx, cfg)
	}
}

func (b *Backfiller) tick(ctx context.Context, cfg *BackfillConfig) {
	jobs, err := b.store.PendingAssetTagJobs(cfg.Batch, cfg.MaxAttempts)
	if err != nil {
		logutil.Warn("读取补标签欠条失败", "err", err)
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		b.process(ctx, job, cfg)
	}
	if removed, err := b.store.CleanupAssetTagJobs(time.Now(), cfg.Retention); err == nil && removed > 0 {
		logutil.Debug("清理已完成标签台账", "removed", removed)
	}
}

func (b *Backfiller) process(ctx context.Context, job state.AssetTagJob, cfg *BackfillConfig) {
	url := strings.TrimSpace(job.URL)
	if url == "" {
		// 欠条没记下原始地址，没地方去看：直接记终态，免得每轮都捞出来空转。
		job.Status = "missing"
		job.LastError = "没有原始地址，文件已消失"
		_ = b.store.UpdateAssetTagJob(job)
		return
	}
	note, err := b.tagger.TagOne(ctx, url, job.Attempts)
	if err != nil {
		job.Attempts++
		if errors.Is(err, ErrSourceGone) {
			// 原始地址打不开了：文件消失是终态，不再重试。
			job.Status = "missing"
			job.LastError = err.Error()
			_ = b.store.UpdateAssetTagJob(job)
			return
		}
		job.Status = "failed"
		job.LastError = err.Error()
		if job.Attempts >= cfg.MaxAttempts {
			logutil.Warn("补标签达到重试上限", "workspace", job.WorkspaceID,
				"group", job.GroupID, "message", job.MessageID, "asset", job.AssetIndex, "err", err)
		}
		_ = b.store.UpdateAssetTagJob(job)
		return
	}
	job.Attempts++
	job.Status = "done"
	job.LastError = ""
	_ = b.store.UpdateAssetTagJob(job)
	logutil.Debug("补标签完成", "workspace", job.WorkspaceID, "message", job.MessageID,
		"asset", job.AssetIndex, "note", note.Note)
}
