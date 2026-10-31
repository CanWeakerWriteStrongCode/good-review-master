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
// QQ 官方在拿不到 mentions 时只给占位值。根因通常是QQ 群设置里没给机器人
// 「查看群所有信息」权限：此时平台不下发 mention 元数据，补上权限后
// mentions[].is_you 会带真实 openid。哨兵只是这种权限缺失时的兜底。
// 这时不能拿账号、AppID 或 robot.id 去填 self.user_id——它们与群内 openid
// 不同命名空间，填进去再补 mention 只是伪造匹配。
// Go 见到哨兵一律判定「不可比较」，只入缓存、不触发回复。
const SentinelSelfID = "unknown_selfid"

// Sender 是发送者的展示信息（OneBot 11 兼容的可选扩展）。
// QQ 官方消息事件本身只有 openid，拿不到昵称；桥接能从 AstrBot 拿到时才会带上。
type Sender struct {
	UserID   string `json:"user_id"`
	Nickname string `json:"nickname"`
	Card     string `json:"card"`
}

// Event 是本项目消费的 OneBot 12 事件（只处理群消息与私聊，其余在入站层丢弃）。
//
// Account 不是协议字段：它来自上报路径 /im/event/{account}，用于和 self.platform
// 拼出工作空间 ID。事件体本身没有任何私有路由字段。
type Event struct {
	ID         string  `json:"id"`
	Time       float64 `json:"time"`
	Type       string  `json:"type"`
	DetailType string  `json:"detail_type"`
	SubType    string  `json:"sub_type"`
	Self       Self    `json:"self"`

	// AccountKind 是平台扩展（OB12 顶层 account_kind）：区分同一个 IM 下的
	// 官方机器人 / 个人号 / App。平台名只表示 IM 软件，不承载账号模式；
	// 工作空间键是 (platform, account_kind, account) 三元组。
	AccountKind string `json:"account_kind"`

	MessageID  string    `json:"message_id"`
	Message    []Segment `json:"message"`
	AltMessage string    `json:"alt_message"`
	GroupID    string    `json:"group_id"`
	UserID     string    `json:"user_id"`

	// Sender 可选：有值时用于学习成员显示名，减少 QQ 官方 openid-only 造成的空昵称。
	Sender *Sender `json:"sender,omitempty"`

	// QQAtMe 是平台扩展（qq.at_me）：实现端明确知道这是一条群 @ 事件，
	// 但拿不到本账号可比较的 openid（群内权限缺 mention 元数据时）。
	// 只在 self.user_id 为哨兵时才采信；授权后走标准 mention 比较。
	QQAtMe bool `json:"qq.at_me"`

	// QQMsgIdx 是平台扩展（qq.msg_idx）：本条消息自己的引用索引（QQ 官方 REFIDX）。
	// 群消息的引用只给被引消息的这个索引，不给 message_id，所以要用它反查。
	QQMsgIdx string `json:"qq.msg_idx"`

	Account string `json:"-"`
}

// Platform 返回事件所属平台；缺失时按 QQ 官方处理。
func (e Event) Platform() string {
	if e.Self.Platform != "" {
		return e.Self.Platform
	}
	return "qq"
}

// Kind 返回账号类型扩展字段（official_bot / personal_account / app）。
// 空串表示实现端没有上报；由工作空间层决定能否匹配已有档案或拒绝新开通。
func (e Event) Kind() string {
	return strings.TrimSpace(e.AccountKind)
}

// IsSelfPlaceholder 判断 self.user_id 是否不可用于 @ 比较。
func IsSelfPlaceholder(userID string) bool {
	switch userID {
	case "", SentinelSelfID, "qq_official":
		return true
	}
	return false
}

// IsPrivate 判断是否私聊（OneBot 12 detail_type=private）。
func (e Event) IsPrivate() bool { return e.DetailType == "private" }

// SenderName 返回发送者展示名：群名片优先，其次账号昵称；都没有时为空串。
// QQ 官方事件基本不带这两个字段，能拿到属于加分项（AstrBot 桥接会透传）。
func (e Event) SenderName() string {
	if e.Sender == nil {
		return ""
	}
	if card := strings.TrimSpace(e.Sender.Card); card != "" {
		return card
	}
	return strings.TrimSpace(e.Sender.Nickname)
}

// ChatID 返回会话键：群聊用 group_id，私聊用 "private:<user_id>"。
// 缓存、人格、锚点、冷却都按这个键隔离，私聊因此天然是一对一会话，
// 与任何群的 ID 都不会撞。
func (e Event) ChatID() string {
	if e.IsPrivate() {
		return "private:" + e.UserID
	}
	return e.GroupID
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

// AdapterAtMe 判断实现端是否明确标记了“本事件 @ 了本账号”。
//
// 这是 qq.at_me 扩展的受控用法：只在 self.user_id 不可比较（哨兵/占位）时采信。
// 有真实 self.user_id 时仍走标准 mention 匹配，扩展不能覆盖它。
func (e Event) AdapterAtMe() bool {
	return e.QQAtMe && e.BotID() == ""
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

// QuotedRefIdx 返回引用段里的平台引用索引（qq.ref_msg_idx）。
//
// QQ 官方群消息的引用不提供被引消息的 message_id，只提供它的 REFIDX；
// 调用方用这个索引在本群已存消息里反查真正的 message_id。
func (e Event) QuotedRefIdx() string {
	for _, seg := range e.Message {
		if seg.Type == SegmentReply {
			return seg.String("qq.ref_msg_idx")
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
