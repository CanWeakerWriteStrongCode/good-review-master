package assets

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"strings"

	// 解码格式注册。
	_ "image/gif"
	_ "image/png"

	"good-review-master/cache"
)

const imageJPEGQuality = 85

// imageDescribeOutput 是描述调用的 JSON 输出。
type imageDescribeOutput struct {
	Content   string `json:"content"`
	InfoLevel string `json:"info_level"`
	HasText   bool   `json:"has_text"`
}

var imageDescribeSchema = json.RawMessage(`{
  "type": "object",
  "properties": {
    "content":    {"type": "string", "description": "图里有什么：类型 + 主题 + 人物/角色的表情情绪，不超过 60 字"},
    "info_level": {"type": "string", "enum": ["high", "medium", "low", "none"], "description": "信息量：high=必须读图内文字/数据，medium=有可用信息，low=几乎没有，none=纯表情/装饰"},
    "has_text":   {"type": "boolean", "description": "图里是否有清晰可读的文字"}
  },
  "required": ["content", "info_level", "has_text"],
  "additionalProperties": false
}`)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func imageID(data []byte) cache.ImageID {
	return cache.ImageID{SHA: sha256Hex(data), Size: int64(len(data))}
}

// probeImage 只读图片头拿宽高与是否动图，不展开像素。
func probeImage(data []byte, mime string) (width, height int, animated bool, err error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return 0, 0, false, err
	}
	return cfg.Width, cfg.Height, mime == "image/gif", nil
}

// downscaleImage 按“长边 + 总像素”双上限等比缩小，并统一编码成 JPEG。
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
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: imageJPEGQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// resizeBox 用面积平均做缩放。
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

// parseImageDescribe 解析描述输出：容忍代码围栏与前后散文，要求 content 非空。
func parseImageDescribe(raw string) (imageDescribeOutput, error) {
	text := stripJSONFences(raw)
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return imageDescribeOutput{}, fmt.Errorf("不是 JSON：%s", truncateLog(raw))
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

func stripJSONFences(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = strings.TrimLeft(s, "`")
		if i := strings.IndexByte(s, '\n'); i != -1 {
			s = s[i+1:]
		}
		if idx := strings.LastIndex(s, "```"); idx != -1 {
			s = s[:idx]
		}
	}
	return strings.TrimSpace(s)
}

func truncateLog(raw string) string {
	runes := []rune(strings.TrimSpace(raw))
	if len(runes) <= 200 {
		return string(runes)
	}
	return string(runes[:200]) + "…"
}
