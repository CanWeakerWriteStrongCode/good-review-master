// Package pipeline 定义三段式消息流水线的阶段契约与缓冲层。
//
// 阶段之间的数据只允许放可序列化的 ID 与值，不放 *Workspace 之类运行时指针：
// 这样现在用进程内实现，以后可以直接换成消息队列，业务代码不用改。
package pipeline

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
)

// 缓冲层分区键。M0 先用单一分区保持现有的全局 FIFO 行为；
// 每会话一个分区在 M4 打开，Key 改成 workspaceID + "|" + chatID。
const (
	SerialKey = "serial"

	StageIngest = "ingest"
	StageSync   = "sync"
	StageAssets = "assets"
	StageAgent  = "agent"
)

// 阶段载荷类型（conversation.Inbound / conversation.Synced）不在这里定义：
// 它们属于会话域，本包只管"怎么把载荷搬过去"。
// 早先 InboundEvent / SyncedMessage / EnrichedMessage 放在这里，
// 导致 workspace 和 assets 为了用值对象反过来 import 编排层。

var itemSeq atomic.Uint64

// NewItem 把阶段载荷编码进 Item。id 为空时用进程内序号兜底，
// 保证 Item.ID 始终非空，方便以后 MQ 做幂等。
func NewItem(stage, key, id string, payload any) (Item, error) {
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
	return Item{
		ID:    stage + "|" + key + "|" + id,
		Key:   key,
		Stage: stage,
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
