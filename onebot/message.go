package onebot

// OneBot 12 消息段类型（只列本项目用到的）。
const (
	SegmentText       = "text"
	SegmentMention    = "mention"
	SegmentMentionAll = "mention_all"
	SegmentImage      = "image"
	SegmentVoice      = "voice"
	SegmentAudio      = "audio"
	SegmentVideo      = "video"
	SegmentFile       = "file"
	SegmentLocation   = "location"
	SegmentReply      = "reply"
)

// Segment 是 OneBot 12 消息段：{type, data}。
//
// 入站时由适配器翻译平台消息；出站时 Go 用它表达文本 / @ / 引用 / 媒体。
// 平台特有信息按 OneBot 12 扩展规则带前缀（QQ 官方媒体只有 URL，用 qq.url）。
type Segment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// String 读取段参数里的字符串值；缺失或类型不对返回空串。
func (s Segment) String(key string) string {
	if s.Data == nil {
		return ""
	}
	value, ok := s.Data[key]
	if !ok {
		return ""
	}
	text, _ := value.(string)
	return text
}

// TextSegment 纯文本段
func TextSegment(text string) Segment {
	return Segment{Type: SegmentText, Data: map[string]any{"text": text}}
}

// MentionSegment @ 群成员。userID 在 QQ 官方场景下是 member_openid。
func MentionSegment(userID string) Segment {
	return Segment{Type: SegmentMention, Data: map[string]any{"user_id": userID}}
}

// ReplySegment 引用回复。messageID 是平台原始消息 ID（OB12 字符串）。
func ReplySegment(messageID string) Segment {
	return Segment{Type: SegmentReply, Data: map[string]any{"message_id": messageID}}
}

// ImageSegment 图片。QQ 官方只有 URL，按扩展规则放 qq.url。
func ImageSegment(url string) Segment {
	return Segment{Type: SegmentImage, Data: map[string]any{"qq.url": url}}
}

// FileSegment 文件。QQ 官方同样用 qq.url + qq.name 扩展。
func FileSegment(name, url string) Segment {
	return Segment{Type: SegmentFile, Data: map[string]any{"qq.url": url, "qq.name": name}}
}

// VoiceSegment 语音。
func VoiceSegment(url string) Segment {
	return Segment{Type: SegmentVoice, Data: map[string]any{"qq.url": url}}
}

// VideoSegment 视频。
func VideoSegment(url string) Segment {
	return Segment{Type: SegmentVideo, Data: map[string]any{"qq.url": url}}
}
