package onebot

import (
	"encoding/json"
	"strings"
)

// Self 是 OneBot 12 的机器人自身标识。平台是协议原生字段：
// QQ 官方与将来自家 OneBot 实现都用 "qq"，账号由入站路径区分。
type Self struct {
	Platform string `json:"platform"`
	UserID   string `json:"user_id"`
}

// SentinelSelfID 表示「本条事件里没有可与 mention.user_id 比较的机器人 ID」。
//
// QQ 官方在拿不到 mentions 时只给占位值；这时不能拿账号、AppID 或 robot.id 去填
// self.user_id——它们与群内 openid 不同命名空间，填进去再补 mention 只是伪造匹配。
// Go 见到哨兵一律判定「不可比较」，只入缓存、不触发回复。
const SentinelSelfID = "unknown_selfid"

// Event 是本项目消费的 OneBot 12 事件（只处理群消息，其余在入站层丢弃）。
//
// Account 不是协议字段：它来自上报路径 /im/event/{account}，用于和 self.platform
// 拼出工作空间 ID。事件体本身没有任何私有路由字段。
type Event struct {
	ID         string    `json:"id"`
	Time       float64   `json:"time"`
	Type       string    `json:"type"`
	DetailType string    `json:"detail_type"`
	SubType    string    `json:"sub_type"`
	Self       Self      `json:"self"`
	MessageID  string    `json:"message_id"`
	Message    []Segment `json:"message"`
	AltMessage string    `json:"alt_message"`
	GroupID    string    `json:"group_id"`
	UserID     string    `json:"user_id"`

	Account string `json:"-"`
}

// Platform 返回事件所属平台；缺失时按 QQ 官方处理。
func (e Event) Platform() string {
	if e.Self.Platform != "" {
		return e.Self.Platform
	}
	return "qq"
}

// IsSelfPlaceholder 判断 self.user_id 是否不可用于 @ 比较。
func IsSelfPlaceholder(userID string) bool {
	switch userID {
	case "", SentinelSelfID, "qq_official":
		return true
	}
	return false
}

// BotID 返回可用于 @ 比较的机器人 ID；不可比较时返回空串。
func (e Event) BotID() string {
	if IsSelfPlaceholder(e.Self.UserID) {
		return ""
	}
	return e.Self.UserID
}

// MentionsBot 判断这条消息是否 @ 了本账号：存在 mention 段且 user_id 与 self.user_id 相等。
//
// 这是 OneBot 12 的标准判定；「收到事件」不等于「被 @」。
func (e Event) MentionsBot() bool {
	botID := e.BotID()
	if botID == "" {
		return false
	}
	for _, seg := range e.Message {
		if seg.Type == SegmentMention && seg.String("user_id") == botID {
			return true
		}
	}
	return false
}

// MentionsAll 判断是否 @ 全体（@全体不等于 @ 本账号，不触发回复）。
func (e Event) MentionsAll() bool {
	for _, seg := range e.Message {
		if seg.Type == SegmentMentionAll {
			return true
		}
	}
	return false
}

// QuotedMessageID 返回被引用消息的 ID；没有引用时为空。
func (e Event) QuotedMessageID() string {
	for _, seg := range e.Message {
		if seg.Type == SegmentReply {
			return seg.String("message_id")
		}
	}
	return ""
}

// Images 返回消息里的图片 URL。QQ 官方只给 URL，按扩展规则放在 data["qq.url"]。
func (e Event) Images() []string {
	var out []string
	for _, seg := range e.Message {
		if seg.Type != SegmentImage {
			continue
		}
		url := seg.String("qq.url")
		if url == "" {
			url = seg.String("url")
		}
		if url != "" {
			out = append(out, url)
		}
	}
	return out
}

// RenderText 把消息段渲染成给缓存和大模型看的文本：@ 统一成「@昵称(ID)」。
//
// nameOf 返回群成员展示名，拿不到时由调用方返回空串（这里回落成「用户」）。
// openid 保留在括号里：昵称会变、也可能重名，模型要靠它判断是不是同一个人。
func (e Event) RenderText(nameOf func(userID string) string) string {
	var b strings.Builder
	for _, seg := range e.Message {
		switch seg.Type {
		case SegmentText:
			b.WriteString(seg.String("text"))
		case SegmentMention:
			id := seg.String("user_id")
			if id == "" {
				b.WriteString("@未知用户")
				continue
			}
			name := ""
			if nameOf != nil {
				name = nameOf(id)
			}
			if name == "" {
				name = "用户"
			}
			b.WriteString("@" + name + "(" + id + ")")
		case SegmentMentionAll:
			b.WriteString("@所有人")
		case SegmentImage:
			b.WriteString("[图片]")
		case SegmentVoice, "record":
			b.WriteString("[语音]")
		case SegmentVideo:
			b.WriteString("[视频]")
		case SegmentFile:
			b.WriteString("[文件]")
		case SegmentLocation:
			b.WriteString("[位置]")
		case SegmentReply:
			// 引用不进入正文，由 reply_id 单独表达。
		default:
			if seg.Type != "" {
				b.WriteString("[" + seg.Type + "]")
			}
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return e.AltMessage
	}
	return text
}

// PlainText 只拼接文本段（不含 @、图片等占位）。用于解析管理指令：
// 指令关键字都在文本里，mention 的渲染结果只会干扰正则。
func (e Event) PlainText() string {
	var b strings.Builder
	for _, seg := range e.Message {
		if seg.Type == SegmentText {
			b.WriteString(seg.String("text"))
		}
	}
	text := strings.TrimSpace(b.String())
	if text == "" {
		return e.AltMessage
	}
	return text
}

// RawJSON 返回事件原文（排障日志用）。仅调试接口构造事件时为空。
func (e Event) RawJSON() string {
	raw, err := json.Marshal(e)
	if err != nil {
		return ""
	}
	return string(raw)
}
