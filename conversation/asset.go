package conversation

// 这里放的是"消息里带的那几个资产"的值对象。打标签的动作在 assets 包，
// 但结果要跟着消息一起走过 assets → agent，所以模型放在会话域。

// 资产整体状态。agent 据此决定是否在 prompt 里追加降级提示。
const (
	StatusOK       = "ok"
	StatusDeferred = "deferred"
	StatusDegraded = "degraded"
	StatusFailed   = "failed"
)

// Asset 一条消息里的一个资产（当前只有图片）。
//
// Index 对齐 Message.Images 的下标：agent 渲染候选图时按下标找回原 URL。
type Asset struct {
	Index   int       `json:"index"`
	Kind    string    `json:"kind"`
	URL     string    `json:"url"`
	ImageID ImageID   `json:"image_id,omitempty"`
	Note    ImageNote `json:"note,omitempty"`
	Status  string    `json:"status"`
	Error   string    `json:"error,omitempty"`
}
