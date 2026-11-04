// Package assets 是 Stage 2：给消息里的图片/视频/文件打标签。
//
// 打标签会下载、解码、调视觉模型，慢且会失败；所以这里的策略是
// “能做就做，做不完先欠着”：成功写进全局图库，没做完的返回给 Stage 2 落欠条，
// 后台 worker 再补。消息本身永远不会因为标签失败而被丢掉。
package assets

import (
	"context"
	"fmt"
	"sync"
	"time"

	"good-review-master/config"
	"good-review-master/conversation"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/mcpserver"
)

// ImageNoteStore 是全局图库的业务侧接口，由 state.Store 实现。
type ImageNoteStore interface {
	GetImageNotes(ids []conversation.ImageID) (map[conversation.ImageID]conversation.ImageNote, error)
	SaveImageNote(note conversation.ImageNote) error
	MarkImageNoteFailed(note conversation.ImageNote) error
}

// ImageTagger 用视觉模型给图片写描述。
type ImageTagger struct {
	cfg     config.Snapshot
	client  llm.Client
	prompt  func() string
	notes   ImageNoteStore
	fetcher func() mcpserver.Fetcher
}

// NewImageTagger 创建图片标签器。cfg 必须是带 settings 覆盖的快照。
func NewImageTagger(cfg config.Snapshot, client llm.Client, prompt func() string, notes ImageNoteStore) *ImageTagger {
	return &ImageTagger{
		cfg:    cfg,
		client: client,
		prompt: prompt,
		notes:  notes,
		fetcher: func() mcpserver.Fetcher {
			return mcpserver.DefaultFetcher(cfg().LLMConfig.ImageLocalDirs...)
		},
	}
}

// DeferredAsset 本次没来得及打标签、需要后台补的资产。
// 只记原始地址：后台按这个 URL（或本地临时文件路径）重新取一次，
// 取不到就把欠条记成 missing，不再另存原图副本。
type DeferredAsset struct {
	Index int
	Kind  string
	URL   string
	Error string
}

// TagResult 一条消息的打标签结果。
type TagResult struct {
	ImageIDs   []conversation.ImageID
	ImageNotes []conversation.ImageNote
	Assets     []conversation.Asset
	Status     string
	Deferred   []DeferredAsset
}

// errBudgetExhausted 一条消息的资产处理预算（下载 + 描述）用完时的统一文案。
const errBudgetExhausted = "标签时间预算耗尽"

// TagURLs 给一条消息里的图片 URL 打标签。
func (t *ImageTagger) TagURLs(ctx context.Context, urls []string) TagResult {
	cfg := t.cfg()
	result := TagResult{Status: conversation.StatusOK}
	if len(urls) == 0 {
		return result
	}
	if cfg.LLMConfig.ImageNoteMaxSide <= 0 || t.notes == nil {
		// 关闭入库描述：不阻塞消息，也不产生欠条。
		return result
	}

	n := len(urls)
	result.ImageIDs = make([]conversation.ImageID, n)
	result.ImageNotes = make([]conversation.ImageNote, n)
	result.Assets = make([]conversation.Asset, n)
	datas := make([][]byte, n)
	mimes := make([]string, n)

	// 预算覆盖「下载 + 描述」整条链路。下载器每张图自带 30s 超时且不受
	// MessageBudget 约束，计时器必须从下载前开始，「一条消息最多多久」才是真上限。
	budgetCtx := ctx
	cancel := func() {}
	if budget := cfg.Assets.MessageBudget; budget > 0 {
		budgetCtx, cancel = context.WithTimeout(ctx, budget)
	}
	defer cancel()

	fetcher := t.fetcher()
	for i, url := range urls {
		asset := conversation.Asset{Index: i, Kind: "image", URL: url, Status: conversation.StatusOK}
		// 预算已耗尽：剩下的图连下载都不再尝试（一张最多还要等 30s），
		// 直接记欠条交给后台补，让消息尽快进 Stage 3。
		if budgetCtx.Err() != nil {
			asset.Status = conversation.StatusDeferred
			asset.Error = errBudgetExhausted
			result.Assets[i] = asset
			result.Deferred = append(result.Deferred, DeferredAsset{
				Index: i, Kind: "image", URL: url, Error: errBudgetExhausted,
			})
			continue
		}
		data, mime, err := fetcher.FetchImage(budgetCtx, url)
		if err != nil {
			// 预算到点导致的失败属于「没排上」，记 deferred 等后台补；
			// 其余（404、不是图片、超限）是真失败。
			asset.Status = conversation.StatusFailed
			if budgetCtx.Err() != nil {
				asset.Status = conversation.StatusDeferred
			}
			asset.Error = err.Error()
			result.Assets[i] = asset
			result.Deferred = append(result.Deferred, DeferredAsset{
				Index: i, Kind: "image", URL: url, Error: err.Error(),
			})
			logutil.Warn("读取入库图片失败，记为欠条", "err", err)
			continue
		}
		datas[i], mimes[i] = data, mime
		result.ImageIDs[i] = imageID(data)
		result.Assets[i] = asset
	}

	existing, err := t.notes.GetImageNotes(result.ImageIDs)
	if err != nil {
		logutil.Warn("读取图片描述失败，按未命中处理", "err", err)
		existing = nil
	}

	needDescribe := make(map[conversation.ImageID][]int)
	for i, id := range result.ImageIDs {
		if id.SHA == "" {
			continue
		}
		note, ok := existing[id]
		if !ok {
			needDescribe[id] = append(needDescribe[id], i)
			continue
		}
		result.ImageNotes[i] = note
		if note.Status == "ok" {
			result.Assets[i].Note = note
			continue
		}
		if note.Attempts < cfg.LLMConfig.ImageNoteRetries {
			needDescribe[id] = append(needDescribe[id], i)
			continue
		}
		result.Assets[i].Note = note
		result.Assets[i].Status = conversation.StatusFailed
		result.Assets[i].Error = "图片描述失败且已达重试上限"
		result.Deferred = append(result.Deferred, DeferredAsset{
			Index: i, Kind: "image", URL: urls[i], Error: result.Assets[i].Error,
		})
	}

	if len(needDescribe) == 0 {
		result.Status = overallStatus(result.Assets)
		return result
	}

	// 信号量按「本次调用（即一条消息）」创建：它限制的是一条消息里的多张图
	// 同时描述几张，不是跨消息的全局上限。全局出站峰值 = Stage 2 的
	// asset_concurrency × 本值；要真正全局限流得把信号量提到 ImageTagger 上。
	limit := cfg.Assets.TagConcurrency
	if limit <= 0 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for id, indexes := range needDescribe {
		wg.Add(1)
		go func(id conversation.ImageID, indexes []int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-budgetCtx.Done():
				mu.Lock()
				for _, i := range indexes {
					result.Assets[i].Status = conversation.StatusDeferred
					result.Assets[i].Error = errBudgetExhausted
					result.Deferred = append(result.Deferred, DeferredAsset{
						Index: i, Kind: "image", URL: urls[i], Error: errBudgetExhausted,
					})
				}
				mu.Unlock()
				return
			}
			defer func() { <-sem }()

			first := indexes[0]
			note, err := t.describeImage(budgetCtx, cfg, datas[first], mimes[first], id, existing[id].Attempts)
			mu.Lock()
			for _, i := range indexes {
				result.ImageNotes[i] = note
				result.Assets[i].Note = note
				if err != nil {
					// 预算取消与普通失败都进欠条；后台会重新判 URL/本地副本。
					status := conversation.StatusFailed
					if budgetCtx.Err() != nil {
						status = conversation.StatusDeferred
					}
					result.Assets[i].Status = status
					result.Assets[i].Error = err.Error()
					result.Deferred = append(result.Deferred, DeferredAsset{
						Index: i, Kind: "image", URL: urls[i], Error: err.Error(),
					})
				}
			}
			mu.Unlock()
		}(id, indexes)
	}
	wg.Wait()
	result.Status = overallStatus(result.Assets)
	return result
}

func overallStatus(assets []conversation.Asset) string {
	status := conversation.StatusOK
	for _, asset := range assets {
		switch asset.Status {
		case conversation.StatusDeferred:
			return conversation.StatusDeferred
		case conversation.StatusFailed, conversation.StatusDegraded:
			status = conversation.StatusFailed
		case "", conversation.StatusOK:
		default:
			status = conversation.StatusDegraded
		}
	}
	return status
}

// describeImage 对一张图做低清描述并写入全局图库。
func (t *ImageTagger) describeImage(ctx context.Context, cfg *config.Config, data []byte, mime string, id conversation.ImageID, attempts int) (conversation.ImageNote, error) {
	if err := ctx.Err(); err != nil {
		return conversation.ImageNote{ImageID: id}, fmt.Errorf("标签时间预算耗尽：%w", err)
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return conversation.ImageNote{ImageID: id}, fmt.Errorf("标签时间预算耗尽")
	}
	note := conversation.ImageNote{
		ImageID:       id,
		MIME:          mime,
		Status:        "failed",
		Attempts:      attempts + 1,
		Model:         cfg.LLMConfig.ModelName,
		PromptVersion: cfg.Assets.PromptVersion,
	}
	if width, height, animated, err := probeImage(data, mime); err == nil {
		note.Width, note.Height, note.Animated = width, height, animated
	}
	small, err := downscaleImage(data, cfg.LLMConfig.ImageNoteMaxSide, cfg.LLMConfig.ImageNoteMaxPixels)
	if err != nil {
		_ = t.notes.MarkImageNoteFailed(note)
		return conversation.ImageNote{ImageID: id}, fmt.Errorf("图片解码/降采样失败：%w", err)
	}
	timeout := time.Duration(cfg.LLMConfig.ImageNoteTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = cfg.LLMTimeout
	}
	noteCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := t.client.ChatStructured(noteCtx, []llm.Message{
		{Role: llm.RoleSystem, Content: t.prompt()},
		{Role: llm.RoleUser, Content: "请描述这张图片。", ImageDataURLs: []string{llm.DataURL("image/jpeg", small)}},
	}, imageDescribeSchema)
	if err != nil {
		_ = t.notes.MarkImageNoteFailed(note)
		return conversation.ImageNote{ImageID: id}, fmt.Errorf("图片描述调用失败：%w", err)
	}
	out, err := parseImageDescribe(resp.Content)
	if err != nil {
		_ = t.notes.MarkImageNoteFailed(note)
		return conversation.ImageNote{ImageID: id}, err
	}
	note.Note = out.Content
	note.InfoLevel = out.InfoLevel
	note.HasText = out.HasText
	note.Status = "ok"
	if err := t.notes.SaveImageNote(note); err != nil {
		return note, fmt.Errorf("写入图片描述失败：%w", err)
	}
	return note, nil
}
