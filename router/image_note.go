package router

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"strings"
	"sync"
	"time"

	// 解码格式注册：入库描述需要先知道图片尺寸/内容，再降采样。
	_ "image/gif"
	_ "image/png"

	"good-review-master/cache"
	"good-review-master/config"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/mcpserver"
)

// 入库图片描述：图片进来时算内容哈希，未命中全局图库就同步调一次低清描述。
// view_image 保持纯工具，描述完全发生在业务层。
const (
	// imageNoteTotalBudget 一条消息里所有图片描述的总时间预算：超出的图按"未知"处理，
	// 不让一条刷图消息把事件队列卡太久。
	imageNoteTotalBudget = 30 * time.Second
	// imageNoteJPEGQuality 描述用图的 JPEG 质量：只需判断类型/主题，不必无损。
	imageNoteJPEGQuality = 85
)

// ImageNoteStore 是全局图库（image_notes 表）的业务侧接口，由 state.Store 实现。
// 接口放在这里而不是直接依赖 state 包：router 只关心"按内容标识读写描述"。
type ImageNoteStore interface {
	GetImageNotes(ids []cache.ImageID) (map[cache.ImageID]cache.ImageNote, error)
	SaveImageNote(note cache.ImageNote) error
	MarkImageNoteFailed(note cache.ImageNote) error
}

// SetImageNoteStore 注入全局图库。nil 表示不做入库描述（保持纯工具看图）。
func (r *Router) SetImageNoteStore(store ImageNoteStore) { r.imageNotes = store }

// imageDescribeOutput 是描述调用的 JSON 输出。
type imageDescribeOutput struct {
	Content   string `json:"content"`
	InfoLevel string `json:"info_level"`
	HasText   bool   `json:"has_text"`
}

var imageDescribeSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "content":    {"type": "string", "description": "图里有什么：类型 + 主题，不超过 60 字"},
    "info_level": {"type": "string", "enum": ["high", "medium", "low", "none"], "description": "信息量：high=必须读图内文字/数据，medium=有可用信息，low=几乎没有，none=纯表情/装饰"},
    "has_text":   {"type": "boolean", "description": "图里是否有清晰可读的文字"}
  },
  "required": ["content", "info_level", "has_text"],
  "additionalProperties": false
}`)

// EnsureImageNotes 为一条消息里的图片补齐描述，返回与 images 对齐的 id 与描述。
// 同步执行：未命中的图会调一次低清描述并写入全局图库；失败一律降级为空描述。
// 关闭入库描述（image_note_max_side=0）、没有图库或没有图片时返回 nil, nil。
func (r *Router) EnsureImageNotes(ctx context.Context, images []string) ([]cache.ImageID, []cache.ImageNote) {
	cfg := r.appCfg()
	if len(images) == 0 || cfg.LLMConfig.ImageNoteMaxSide <= 0 || r.imageNotes == nil {
		return nil, nil
	}

	ids := make([]cache.ImageID, len(images))
	notes := make([]cache.ImageNote, len(images))
	datas := make([][]byte, len(images))
	mimes := make([]string, len(images))
	fetcher := mcpserver.DefaultFetcher(cfg.LLMConfig.ImageLocalDirs...)
	for i, path := range images {
		data, mime, err := fetcher.FetchImage(ctx, path)
		if err != nil {
			logutil.Warn("读取入库图片失败，跳过描述", "err", err)
			continue
		}
		datas[i], mimes[i] = data, mime
		ids[i] = cache.ImageID{SHA: sha256Hex(data), Size: int64(len(data))}
	}

	existing, err := r.imageNotes.GetImageNotes(ids)
	if err != nil {
		logutil.Warn("读取图片描述失败，按未命中处理", "err", err)
		existing = nil
	}
	for i, id := range ids {
		if note, ok := existing[id]; ok {
			notes[i] = note
		}
	}

	// 未命中、以及失败但还有重试次数的图，才需要调描述。
	// 同一张图在一条消息里出现多次时只描述一次，结果复用到所有下标。
	needDescribe := make(map[cache.ImageID][]int)
	for i, id := range ids {
		if id.SHA == "" {
			continue
		}
		note, ok := existing[id]
		if !ok {
			needDescribe[id] = append(needDescribe[id], i)
			continue
		}
		if note.Status != "ok" && note.Attempts < cfg.LLMConfig.ImageNoteRetries {
			needDescribe[id] = append(needDescribe[id], i)
		}
	}

	budgetCtx, cancel := context.WithTimeout(ctx, imageNoteTotalBudget)
	defer cancel()
	limit := cfg.LLMConfig.ImageNoteConcurrency
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
				return
			}
			defer func() { <-sem }()
			first := indexes[0]
			note := r.describeImage(budgetCtx, cfg, datas[first], mimes[first], id, existing[id].Attempts)
			mu.Lock()
			for _, i := range indexes {
				notes[i] = note
			}
			mu.Unlock()
		}(id, indexes)
	}
	wg.Wait()
	return ids, notes
}

// describeImage 对一张图做低清描述并写入全局图库。失败返回空描述（调用方按未知处理）。
func (r *Router) describeImage(ctx context.Context, cfg *config.Config, data []byte, mime string, id cache.ImageID, attempts int) cache.ImageNote {
	note := cache.ImageNote{
		ImageID:  id,
		MIME:     mime,
		Status:   "failed",
		Attempts: attempts + 1,
		Model:    cfg.LLMConfig.ModelName,
	}
	if width, height, animated, err := probeImage(data, mime); err == nil {
		note.Width, note.Height, note.Animated = width, height, animated
	}

	small, err := downscaleImage(data, cfg.LLMConfig.ImageNoteMaxSide, cfg.LLMConfig.ImageNoteMaxPixels)
	if err != nil {
		logutil.Warn("图片解码/降采样失败，跳过描述", "err", err, "mime", mime)
		_ = r.imageNotes.MarkImageNoteFailed(note)
		return cache.ImageNote{ImageID: id}
	}

	timeout := time.Duration(cfg.LLMConfig.ImageNoteTimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = cfg.LLMTimeout
	}
	noteCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := r.llmClient.ChatStructured(noteCtx, []llm.Message{
		{Role: llm.RoleSystem, Content: r.promptCfg.ImageDescribePrompt()},
		{Role: llm.RoleUser, Content: "请描述这张图片。", ImageDataURLs: []string{llm.DataURL("image/jpeg", small)}},
	}, imageDescribeSchema)
	if err != nil {
		logutil.Warn("图片描述调用失败", "err", err)
		_ = r.imageNotes.MarkImageNoteFailed(note)
		return cache.ImageNote{ImageID: id}
	}
	out, err := parseImageDescribe(resp.Content)
	if err != nil {
		logutil.Warn("图片描述结果无法解析", "err", err)
		_ = r.imageNotes.MarkImageNoteFailed(note)
		return cache.ImageNote{ImageID: id}
	}

	note.Note = out.Content
	note.InfoLevel = out.InfoLevel
	note.HasText = out.HasText
	note.Status = "ok"
	if err := r.imageNotes.SaveImageNote(note); err != nil {
		logutil.Warn("写入图片描述失败", "err", err)
	}
	return note
}

// parseImageDescribe 解析描述输出：容忍代码围栏与前后散文，要求 content 非空。
func parseImageDescribe(raw string) (imageDescribeOutput, error) {
	text := stripJSONFences(raw)
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return imageDescribeOutput{}, fmt.Errorf("不是 JSON：%s", truncateJudgeLog(raw))
	}
	var out imageDescribeOutput
	if err := json.Unmarshal([]byte(text[start:end+1]), &out); err != nil {
		return imageDescribeOutput{}, fmt.Errorf("解析失败：%w", err)
	}
	out.Content = strings.TrimSpace(out.Content)
	if out.Content == "" {
		return imageDescribeOutput{}, fmt.Errorf("content 为空")
	}
	switch out.InfoLevel {
	case "high", "medium", "low", "none":
	default:
		out.InfoLevel = "unknown"
	}
	return out, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// probeImage 只读图片头拿宽高与是否动图，不展开像素。
func probeImage(data []byte, mime string) (width, height int, animated bool, err error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false, err
	}
	return cfg.Width, cfg.Height, mime == "image/gif", nil
}

// downscaleImage 按"长边 + 总像素"双上限等比缩小，并统一编码成 JPEG。
// 只用于描述：长边 640 / 40 万像素足够判断类型和主题，远低于全分辨率截图。
func downscaleImage(data []byte, maxSide, maxPixels int) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	bounds := img.Bounds()
	srcW, srcH := bounds.Dx(), bounds.Dy()
	if srcW <= 0 || srcH <= 0 {
		return nil, fmt.Errorf("图片尺寸无效：%dx%d", srcW, srcH)
	}

	scale := 1.0
	if maxSide > 0 {
		if longest := max(srcW, srcH); longest > maxSide {
			scale = math.Min(scale, float64(maxSide)/float64(longest))
		}
	}
	if maxPixels > 0 && srcW*srcH > maxPixels {
		scale = math.Min(scale, math.Sqrt(float64(maxPixels)/float64(srcW*srcH)))
	}
	dstW := max(1, int(math.Round(float64(srcW)*scale)))
	dstH := max(1, int(math.Round(float64(srcH)*scale)))

	dst := resizeBox(img, bounds, dstW, dstH)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: imageNoteJPEGQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// resizeBox 用面积平均做缩放：比最近邻清晰得多（截图里的字不会糊成锯齿），
// 又不引入额外依赖。
func resizeBox(src image.Image, srcBounds image.Rectangle, dstW, dstH int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	srcW, srcH := srcBounds.Dx(), srcBounds.Dy()
	for y := 0; y < dstH; y++ {
		sy0 := srcBounds.Min.Y + y*srcH/dstH
		sy1 := srcBounds.Min.Y + (y+1)*srcH/dstH
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for x := 0; x < dstW; x++ {
			sx0 := srcBounds.Min.X + x*srcW/dstW
			sx1 := srcBounds.Min.X + (x+1)*srcW/dstW
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var r, g, b, a, n uint64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					r += uint64(cr)
					g += uint64(cg)
					b += uint64(cb)
					a += uint64(ca)
					n++
				}
			}
			if n == 0 {
				continue
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(r / n >> 8),
				G: uint8(g / n >> 8),
				B: uint8(b / n >> 8),
				A: uint8(a / n >> 8),
			})
		}
	}
	return dst
}
