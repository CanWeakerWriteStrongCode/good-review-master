package onebot

import (
	"encoding/json"
	"strconv"
)

// Event OneBot 11 群消息事件（本项目只消费这一种）。
//
// 除标准字段外只保留两个扩展：
//   - display_message：桥接层把各平台 @ 语法统一成「@昵称(稳定ID)」后的文本；
//   - mentioned：桥接层对"这条消息是否在叫机器人"的结论。
//
// 其余平台差异（引用、群名、机器人身份）都走 OneBot 11 标准表示：
// 引用是 reply 消息段，机器人身份是 self_id，群名走 get_group_info 动作。
type Event struct {
	PostType    string          `json:"post_type"`
	MessageType string          `json:"message_type"`
	GroupID     string          `json:"group_id"`
	UserID      string          `json:"user_id"`
	Nickname    string          `json:"nickname"`
	Card        string          `json:"card"`
	Message     json.RawMessage `json:"message"`
	RawMessage  string          `json:"raw_message"`
	// DisplayMessage 是桥接插件翻译后的可读文本：@ 已统一成「@昵称(稳定ID)」。
	// RawMessage 保留平台原样用于排障；缓存和大模型只读 DisplayMessage。
	DisplayMessage string `json:"display_message"`
	MessageID      int64  `json:"message_id"`
	Time           int64  `json:"time"`
	SelfID         string `json:"self_id"`
	// Mentioned 由桥接层给出"这条消息是否在叫机器人"的判断。
	// QQ 官方机器人的 @ 语义和 OneBot 的 CQ 码不同（group_openid/mentions），
	// 让适配器把判断结果带进来，业务层就不必认识各平台的 @ 表示。
	Mentioned bool `json:"mentioned"`
}

// ReplyID 返回引用这条消息时应带的 ID。
// 就是标准 message_id：桥接层出站时再把它映射回平台的引用 ID（如 QQ 官方的 REFIDX）。
func (e Event) ReplyID() string {
	if e.MessageID != 0 {
		return strconv.FormatInt(e.MessageID, 10)
	}
	return ""
}

// BotID 返回机器人的稳定 ID。
// "qq_official" / "unknown_selfid" 是适配器的占位值，不代表任何真实身份。
func (e Event) BotID() string {
	switch e.SelfID {
	case "", "qq_official", "unknown_selfid":
		return ""
	}
	return e.SelfID
}

// QuoteMessageID 返回被引用消息在本项目里的 msg_id（没有引用时为 0）。
// 引用是标准 OneBot 11 的 reply 消息段；桥接层把段里的 id 换成与入站 message_id
// 同一套的 int64，因此能直接对上 chat log 的 msg_id。
func (e Event) QuoteMessageID() int64 {
	var segments []struct {
		Type string `json:"type"`
		Data struct {
			ID json.Number `json:"id"`
		} `json:"data"`
	}
	if len(e.Message) > 0 && json.Unmarshal(e.Message, &segments) == nil {
		for _, segment := range segments {
			if segment.Type != "reply" {
				continue
			}
			if id, err := strconv.ParseInt(segment.Data.ID.String(), 10, 64); err == nil {
				return id
			}
		}
	}
	// message 缺失或不是段数组时退回 raw_message 里的 CQ 码。
	return ParseCQReplyID(e.RawMessage)
}

// HistoryMsg 群消息历史中的单条消息
type HistoryMsg struct {
	MessageID int64 `json:"message_id"`
	GroupID   int64 `json:"group_id"`
	UserID    int64 `json:"user_id"`
	Sender    struct {
		Nickname string `json:"nickname"`
		Card     string `json:"card"`
	} `json:"sender"`
	RawMessage string `json:"raw_message"`
	Time       int64  `json:"time"`
}

// LoginInfo get_login_info 响应
type LoginInfo struct {
	UserID   int64  `json:"user_id"`
	Nickname string `json:"nickname"`
}

// GroupInfo get_group_info 响应。
// 只保留群名：QQ 官方机器人的 group_id 是 group_openid 字符串，aiocqhttp 是数字，
// 统一成 int64 会有一边解不出来；调用方本来也只需要群名。
type GroupInfo struct {
	GroupName string `json:"group_name"`
}

// FormatGroupID int64群号转字符串
func FormatGroupID(id int64) string {
	return strconv.FormatInt(id, 10)
}
