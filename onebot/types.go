package onebot

import (
	"encoding/json"
	"strconv"
)

// Event OneBot 事件（简化版）
type Event struct {
	PostType    string          `json:"post_type"`
	MessageType string          `json:"message_type"`
	GroupID     string          `json:"group_id"`
	UserID      string          `json:"user_id"`
	Nickname    string          `json:"nickname"`
	Card        string          `json:"card"`
	Message     json.RawMessage `json:"message"`
	RawMessage  string          `json:"raw_message"`
	MessageID   int64
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

// GroupInfo get_group_info 响应
type GroupInfo struct {
	GroupID   int64  `json:"group_id"`
	GroupName string `json:"group_name"`
}

// FormatGroupID int64群号转字符串
func FormatGroupID(id int64) string {
	return strconv.FormatInt(id, 10)
}
