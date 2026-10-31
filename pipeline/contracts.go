// Package pipeline 定义三段式消息流水线的阶段契约与缓冲层。
//
// 阶段之间的数据只允许放可序列化的 ID 与值，不放 *Workspace 之类运行时指针：
// 这样现在用进程内实现，以后可以直接换成消息队列，业务代码不用改。
package pipeline

import (
	"encoding/json"
	"fmt"
	"sync/atomic"

	"good-review-master/cache"
	"good-review-master/onebot"
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

// InboundEvent 是 B1（入站 → Stage 1）的载荷。
type InboundEvent struct {
	WorkspaceID string       `json:"workspace_id"`
	Event       onebot.Event `json:"event"`
}

// SyncedMessage 是 Stage 1 的输出：身份、引用、正文都已补全，尚未打资产标签。
type SyncedMessage struct {
	WorkspaceID string        `json:"workspace_id"`
	ChatID      string        `json:"chat_id"`
	Private     bool          `json:"private"`
	Event       onebot.Event  `json:"event"` // Stage 3 路由/引用/@ 判定需要原始事件
	Message     cache.Message `json:"message"`
}

// ChatKey 是每会话分区键：同一群的顺序由它保证，不同群之间可以并行。
func ChatKey(workspaceID, chatID string) string {
	return workspaceID + "|" + chatID
}

// 资产整体状态。Stage 3 据此决定是否在 prompt 里追加降级提示。
const (
	AssetStatusOK       = "ok"
	AssetStatusDeferred = "deferred"
	AssetStatusDegraded = "degraded"
	AssetStatusFailed   = "failed"
)

// Asset 一条消息里的一个资产（当前只有图片）。
type Asset struct {
	Index   int             `json:"index"`
	Kind    string          `json:"kind"`
	URL     string          `json:"url"`
	ImageID cache.ImageID   `json:"image_id,omitempty"`
	Note    cache.ImageNote `json:"note,omitempty"`
	Status  string          `json:"status"`
	Error   string          `json:"error,omitempty"`
}

// EnrichedMessage 是 Stage 2 的输出，也是 Stage 3 的输入。
// ImageNotes 单独放：cache.Message 里的同名字段是 json:"-"，不能靠它过缓冲。
type EnrichedMessage struct {
	SyncedMessage
	ImageIDs    []cache.ImageID   `json:"image_ids,omitempty"`
	ImageNotes  []cache.ImageNote `json:"image_notes,omitempty"`
	Assets      []Asset           `json:"assets,omitempty"`
	AssetStatus string            `json:"asset_status,omitempty"`
}

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
