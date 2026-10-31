package assets

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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
		return cache.ImageNote{}, err
	}
	id := imageID(data)
	if existing, err := t.notes.GetImageNotes([]cache.ImageID{id}); err == nil {
		if note, ok := existing[id]; ok && note.Status == "ok" {
			return note, nil
		}
	}
	return t.describeImage(ctx, cfg, data, mime, id, attempts)
}

// SaveDeferredBytes 把已经取到的原图复制到本地缓存，返回欠条要记的路径。
// maxBytes<=0 表示不保存副本。
func SaveDeferredBytes(dir string, id cache.ImageID, data []byte, maxBytes int64) (string, error) {
	if dir == "" || len(data) == 0 || maxBytes <= 0 || int64(len(data)) > maxBytes {
		return "", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s_%d.img", id.SHA, id.Size))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// CleanupDeferredBytes 删除超过 TTL 的原图副本；ttl<=0 表示不过期。
func CleanupDeferredBytes(dir string, ttl time.Duration, now time.Time) int {
	if dir == "" || ttl <= 0 {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cutoff := now.Add(-ttl)
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, entry.Name())); err == nil {
			removed++
		}
	}
	return removed
}

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
	CacheDir    string
	CacheTTL    time.Duration
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
	if removed := CleanupDeferredBytes(cfg.CacheDir, cfg.CacheTTL, time.Now()); removed > 0 {
		logutil.Debug("清理过期原图副本", "removed", removed)
	}
}

func (b *Backfiller) process(ctx context.Context, job state.AssetTagJob, cfg *BackfillConfig) {
	url := job.URL
	if job.LocalPath != "" {
		if _, err := os.Stat(job.LocalPath); err == nil {
			url = job.LocalPath
		} else if url == "" {
			// 本地副本与 URL 都没了：文件消失是终态。
			job.Status = "missing"
			job.LastError = "本地副本不存在，且没有可重试的 URL"
			_ = b.store.UpdateAssetTagJob(job)
			return
		}
	}
	note, err := b.tagger.TagOne(ctx, url, job.Attempts)
	if err != nil {
		job.Attempts++
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
