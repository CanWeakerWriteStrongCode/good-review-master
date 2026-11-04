// Package pipeline 定义各域之间交接的契约与缓冲层：编排层把消息编码成 Item
// 放进 Buffer，下一个域取一批处理。
//
// 本包不知道这个项目有几个域、它们之间怎么接——那是编排层的事（`bot` 的
// `nodes` / `edges`）。这里只认“节点名”这个字符串。
//
// 阶段之间的数据只允许放可序列化的 ID 与值，不放 *Workspace 之类运行时指针：
// 这样现在用进程内实现，以后可以直接换成消息队列，业务代码不用改。
package pipeline

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
)

// SerialKey 是 M0 的单一分区键：所有消息共用一条全局 FIFO。
// 每会话一个分区在 M4 打开，Key 改成 workspaceID + "|" + chatID。
const SerialKey = "serial"

// 节点名。Item.Stage 记的永远是消息将要去的那个节点，而不是造出它的那个节点——
// 幂等键与告警分类都按它算。
//
// 本包不知道有几个节点、它们怎么接：那是编排层的事（`bot` 的 workflows）。
// StageSync 现在指前置（入口 + 补名），沿用旧名是为了面板指标和 dead_letter_sync 不用改。
//
// 接 MQ 时这两级的对应关系：Stage 是 topic（谁消费），Item.Tag 是 tag（哪条工作流）。
const (
	StageSync    = "sync"
	StageAssets  = "assets"
	StageAgent   = "agent"
	StageCommand = "command"
)

// 阶段载荷类型（conversation.Inbound / conversation.Synced）不在这里定义：
// 它们属于会话域，本包只管"怎么把载荷搬过去"。
// 早先 InboundEvent / SyncedMessage / EnrichedMessage 放在这里，
// 导致 workspace 和 assets 为了用值对象反过来 import 编排层。

var itemSeq atomic.Uint64

// NewItem 把阶段载荷编码进 Item。tag 是“这条消息属于哪条工作流”，前置阶段还没有
// 定，传空字符串；定了之后每一跳都带着走，MQ 落地时它就是消息的 tag。
// id 为空时用进程内序号兜底，保证 Item.ID 始终非空，方便以后 MQ 做幂等。
func NewItem(stage, tag, key, id string, payload any) (Item, error) {
	if stage == "" || key == "" {
		return Item{}, fmt.Errorf("pipeline: stage/key 不能为空")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Item{}, fmt.Errorf("pipeline: 编码 %s 载荷失败：%w", stage, err)
	}
	if id == "" {
		id = fmt.Sprintf("%d", itemSeq.Add(1))
	}
	// 前置阶段没有 tag，不拼一段空的进去。
	itemID := stage + "|" + key + "|" + id
	if tag != "" {
		itemID = stage + "|" + tag + "|" + key + "|" + id
	}
	return Item{
		ID:    itemID,
		Key:   key,
		Stage: stage,
		Tag:   tag,
		Body:  body,
	}, nil
}

// DecodeItem 解出阶段载荷。
func DecodeItem[T any](item Item) (T, error) {
	var out T
	if err := json.Unmarshal(item.Body, &out); err != nil {
		return out, fmt.Errorf("pipeline: 解码 %s 载荷失败：%w", item.Stage, err)
	}
	return out, nil
}
