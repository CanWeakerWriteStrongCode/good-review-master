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

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/mcpserver"
	"good-review-master/pipeline"
)

// ImageNoteStore 是全局图库的业务侧接口，由 state.Store 实现。
type ImageNoteStore interface {
	GetImageNotes(ids []cache.ImageID) (map[cache.ImageID]cache.ImageNote, error)
	SaveImageNote(note cache.ImageNote) error
	MarkImageNoteFailed(note cache.ImageNote) error
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
// Data 是已经取到的原始字节，Stage 2 会尽量复制到本地缓存；为空表示只能靠 URL。
type DeferredAsset struct {
	Index int
	Kind  string
	URL   string
	Data  []byte
	Error string
}

// TagResult 一条消息的打标签结果。
type TagResult struct {
	ImageIDs   []cache.ImageID
	ImageNotes []cache.ImageNote
	Assets     []pipeline.Asset
	Status     string
	Deferred   []DeferredAsset
}

// TagURLs 给一条消息里的图片 URL 打标签。
func (t *ImageTagger) TagURLs(ctx context.Context, urls []string) TagResult {
	cfg := t.cfg()
	result := TagResult{Status: pipeline.AssetStatusOK}
	if len(urls) == 0 {
		return result
	}
	if cfg.LLMConfig.ImageNoteMaxSide <= 0 || t.notes == nil {
		// 关闭入库描述：不阻塞消息，也不产生欠条。
		return result
	}

	n := len(urls)
	result.ImageIDs = make([]cache.ImageID, n)
	result.ImageNotes = make([]cache.ImageNote, n)
	result.Assets = make([]pipeline.Asset, n)
	datas := make([][]byte, n)
	mimes := make([]string, n)

	fetcher := t.fetcher()
	for i, url := range urls {
		asset := pipeline.Asset{Index: i, Kind: "image", URL: url, Status: pipeline.AssetStatusOK}
		data, mime, err := fetcher.FetchImage(ctx, url)
		if err != nil {
			asset.Status = pipeline.AssetStatusFailed
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

	needDescribe := make(map[cache.ImageID][]int)
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
		result.Assets[i].Status = pipeline.AssetStatusFailed
		result.Assets[i].Error = "图片描述失败且已达重试上限"
		result.Deferred = append(result.Deferred, DeferredAsset{
			Index: i, Kind: "image", URL: urls[i], Data: datas[i], Error: result.Assets[i].Error,
		})
	}

	if len(needDescribe) == 0 {
		result.Status = overallStatus(result.Assets)
		return result
	}

	budgetCtx := ctx
	cancel := func() {}
	if budget := cfg.Assets.MessageBudget; budget > 0 {
		budgetCtx, cancel = context.WithTimeout(ctx, budget)
	}
	defer cancel()

	limit := cfg.Assets.TagConcurrency
	if limit <= 0 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	var mu sync.Mutex
	for id, indexes := range needDescribe {
		wg.Add(1)
		go func(id cache.ImageID, indexes []int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-budgetCtx.Done():
				mu.Lock()
				for _, i := range indexes {
					result.Assets[i].Status = pipeline.AssetStatusDeferred
					result.Assets[i].Error = "标签时间预算耗尽"
					result.Deferred = append(result.Deferred, DeferredAsset{
						Index: i, Kind: "image", URL: urls[i], Data: datas[i], Error: "标签时间预算耗尽",
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
					status := pipeline.AssetStatusFailed
					if budgetCtx.Err() != nil {
						status = pipeline.AssetStatusDeferred
					}
					result.Assets[i].Status = status
					result.Assets[i].Error = err.Error()
					result.Deferred = append(result.Deferred, DeferredAsset{
						Index: i, Kind: "image", URL: urls[i], Data: datas[i], Error: err.Error(),
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

func overallStatus(assets []pipeline.Asset) string {
	status := pipeline.AssetStatusOK
	for _, asset := range assets {
		switch asset.Status {
		case pipeline.AssetStatusDeferred:
			return pipeline.AssetStatusDeferred
		case pipeline.AssetStatusFailed, pipeline.AssetStatusDegraded:
			status = pipeline.AssetStatusFailed
		case "", pipeline.AssetStatusOK:
		default:
			status = pipeline.AssetStatusDegraded
		}
	}
	return status
}

// describeImage 对一张图做低清描述并写入全局图库。
func (t *ImageTagger) describeImage(ctx context.Context, cfg *config.Config, data []byte, mime string, id cache.ImageID, attempts int) (cache.ImageNote, error) {
	if err := ctx.Err(); err != nil {
		return cache.ImageNote{ImageID: id}, fmt.Errorf("标签时间预算耗尽：%w", err)
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return cache.ImageNote{ImageID: id}, fmt.Errorf("标签时间预算耗尽")
	}
	note := cache.ImageNote{
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
		return cache.ImageNote{ImageID: id}, fmt.Errorf("图片解码/降采样失败：%w", err)
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
		return cache.ImageNote{ImageID: id}, fmt.Errorf("图片描述调用失败：%w", err)
	}
	out, err := parseImageDescribe(resp.Content)
	if err != nil {
		_ = t.notes.MarkImageNoteFailed(note)
		return cache.ImageNote{ImageID: id}, err
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
