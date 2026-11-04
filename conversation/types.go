// Package conversation 是会话域：谁在哪个会话里说了什么，
// 以及三段流水线之间传的载荷。
//
// 这里只放可序列化的值，不放 *Workspace 之类的运行时指针——
// 以后把进程内缓冲换成消息队列时，载荷不用改。
package conversation

import (
	"good-review-master/onebot"
)

// ChatKey 是每会话分区键：同一群的顺序由它保证，不同群之间可以并行。
func ChatKey(workspaceID, chatID string) string {
	return workspaceID + "|" + chatID
}

// Inbound 是 前置（sync）的载荷。
type Inbound struct {
	WorkspaceID string       `json:"workspace_id"`
	Event       onebot.Event `json:"event"`
}

// Synced 是一条消息走完全程的载荷。
//
// sync 填 Message（补名、补引用、入库）；command 执行门控指令并可能填 CommandRan；
// assets 补 Message.ImageIDs 与 ImageNotes，再填 Assets / AssetStatus；agent 只读。
//
// 以前是 SyncedMessage 和 EnrichedMessage 两个类型，后者只是为了绕开
// Message.ImageNotes 上的 json:"-"，于是把同名字段又抄了一份。
// 现在 ImageNotes 正常参与序列化，一个值对象走到底。
type Synced struct {
	WorkspaceID string       `json:"workspace_id"`
	ChatID      string       `json:"chat_id"`
	Private     bool         `json:"private"`
	Event       onebot.Event `json:"event"` // agent 路由/引用/@ 判定需要原始事件
	Message     Message      `json:"message"`

	// Assets 本条消息的资产标签结果，与 Message.Images 一一对齐。
	Assets []Asset `json:"assets,omitempty"`
	// AssetStatus 本条消息资产的整体状态，agent 据此追加降级提示。
	AssetStatus string `json:"asset_status,omitempty"`

	// CommandRan 本条消息的门控指令已由 command 域执行过，值为命中的关键字。
	//
	// agent 看到非空就不再走 RouteMessageNow（那会二次执行并吞掉这句话），
	// 而是直接按当前人格直答。空字符串 = 没有指令执行过，走正常分发。
	CommandRan string `json:"command_ran,omitempty"`
}
