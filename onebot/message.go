package onebot

// Segment 是 OneBot 11 消息段，也是 Go → AstrBot 桥接插件出站消息的统一格式。
// 桥接插件按目标平台把它翻译成 AstrBot 消息链或官方接口字段：
// 文本 / @ / 引用 / 图片 / 语音 / 视频 / 文件。
type Segment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// TextSegment 纯文本段
func TextSegment(text string) Segment {
	return Segment{Type: "text", Data: map[string]any{"text": text}}
}

// AtSegment @ 群成员。qq 在 QQ 官方机器人场景下是 member_openid。
func AtSegment(qq string) Segment {
	return Segment{Type: "at", Data: map[string]any{"qq": qq}}
}

// ReplySegment 引用回复。id 是本项目的 message_id；
// 桥接层出站时再映射回平台自己的引用 ID（如 QQ 官方的 REFIDX）。
func ReplySegment(id string) Segment {
	return Segment{Type: "reply", Data: map[string]any{"id": id}}
}

// ImageSegment 图片。url 可以是 http(s) URL、本地路径或 base64:// 数据。
func ImageSegment(url string) Segment {
	return Segment{Type: "image", Data: map[string]any{"file": url, "url": url}}
}

// FileSegment 文件。name 是展示名，url 可以是 http(s) URL 或本地路径。
func FileSegment(name, url string) Segment {
	return Segment{Type: "file", Data: map[string]any{"name": name, "file": url, "url": url}}
}

// RecordSegment 语音。url 可以是 http(s) URL、本地路径或 base64:// 数据。
func RecordSegment(url string) Segment {
	return Segment{Type: "record", Data: map[string]any{"file": url, "url": url}}
}

// VideoSegment 视频。url 可以是 http(s) URL 或本地路径。
func VideoSegment(url string) Segment {
	return Segment{Type: "video", Data: map[string]any{"file": url, "url": url}}
}

// MarkdownSegment Markdown 文本（仅 AstrBot 的 QQ 官方适配器支持，失败会回退为纯文本）。
func MarkdownSegment(text string) Segment {
	return Segment{Type: "markdown", Data: map[string]any{"text": text}}
}
