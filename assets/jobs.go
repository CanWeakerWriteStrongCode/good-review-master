package assets

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"good-review-master/conversation"
	"good-review-master/logutil"
	"good-review-master/state"
)

// JobStore 是欠条与图库的持久化边界，由 state.Store 实现。
type JobStore interface {
	ClaimableAssetTagJobs(limit int, claim state.AssetTagClaim) ([]state.AssetTagJob, error)
	ClaimAssetTagJob(job state.AssetTagJob, claim state.AssetTagClaim) (bool, error)
	UpdateAssetTagJob(job state.AssetTagJob) error
	SaveImageNote(note conversation.ImageNote) error
	CleanupAssetTagJobs(now time.Time, retention time.Duration) (int64, error)
}

// TagOne 给单个 URL 打标签（后台补标签用）。成功时返回图库记录。
func (t *ImageTagger) TagOne(ctx context.Context, url string, attempts int) (conversation.ImageNote, error) {
	cfg := t.cfg()
	data, mime, err := t.fetcher().FetchImage(ctx, url)
	if err != nil {
		return conversation.ImageNote{}, fmt.Errorf("%w: %v", ErrSourceGone, err)
	}
	id := imageID(data)
	if existing, err := t.notes.GetImageNotes([]conversation.ImageID{id}); err == nil {
		if note, ok := existing[id]; ok && note.Status == "ok" {
			return note, nil
		}
	}
	return t.describeImage(ctx, cfg, data, mime, id, attempts)
}

// ErrSourceGone 表示原始地址已经取不到（本地临时文件被清掉、链接过期）。
// 重试没意义，后台直接把欠条记成 missing 终态。
var ErrSourceGone = errors.New("原始地址取不到")

// Backfiller 常驻后台补标签：定期扫欠条，抢到哪条补哪条。
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
	// ClaimTimeout 是一条欠条被领走后多久没动静，就当作领它的进程已经没了。
	// 必须大于单条最坏耗时（取图 30 秒 + 一次图片描述超时），否则会把
	// 还在干活的副本手上的欠条抢走，同一张图被描述两次。
	ClaimTimeout time.Duration
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
	now := time.Now()
	claim := state.AssetTagClaim{
		Now:         now,
		StaleBefore: now.Add(-cfg.ClaimTimeout),
		MaxAttempts: cfg.MaxAttempts,
	}
	jobs, err := b.store.ClaimableAssetTagJobs(cfg.Batch, claim)
	if err != nil {
		logutil.Warn("读取补标签欠条失败", "err", err)
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		// 候选名单是“看”出来的，不是“占”下来的：几个副本会同时看到同一批。
		// 这一条件更新只有一个能成功，抢不到的说明别人正在补，跳过去就行。
		claimed, err := b.store.ClaimAssetTagJob(job, claim)
		if err != nil {
			logutil.Warn("抢占补标签欠条失败", "workspace", job.WorkspaceID,
				"message", job.MessageID, "asset", job.AssetIndex, "err", err)
			continue
		}
		if !claimed {
			continue
		}
		job.Attempts++ // 库里的是抢到那一刻加的，内存里跟着走，日志和图库看到的才是同一次
		b.process(ctx, job, cfg)
	}
	if removed, err := b.store.CleanupAssetTagJobs(time.Now(), cfg.Retention); err == nil && removed > 0 {
		logutil.Debug("清理已完成标签台账", "removed", removed)
	}
}

func (b *Backfiller) process(ctx context.Context, job state.AssetTagJob, cfg *BackfillConfig) {
	if ctx.Err() != nil {
		// 正在关进程：这条不回写，留在 running 上。租约到期后别的副本（或者下次启动）
		// 会把它重新领走，不会白白吃掉一次重试机会。
		return
	}
	url := strings.TrimSpace(job.URL)
	if url == "" {
		// 欠条没记下原始地址，没地方去看：直接记终态，免得每轮都捞出来空转。
		job.Status = "missing"
		job.LastError = "没有原始地址，文件已消失"
		_ = b.store.UpdateAssetTagJob(job)
		return
	}
	// TagOne 的 attempts 参数是“这次之前试过几次”（它自己会 +1 再写进图库）。
	// 领取时 attempts 已经加到“含这一次”，所以这里退一个，图库里记的次数跟以前一致。
	tries := job.Attempts - 1
	if tries < 0 {
		tries = 0
	}
	note, err := b.tagger.TagOne(ctx, url, tries)
	if err != nil {
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
	job.Status = "done"
	job.LastError = ""
	_ = b.store.UpdateAssetTagJob(job)
	logutil.Debug("补标签完成", "workspace", job.WorkspaceID, "message", job.MessageID,
		"asset", job.AssetIndex, "note", note.Note)
}
