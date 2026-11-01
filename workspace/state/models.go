package state

// 这些是 GORM 写入路径用的行模型。读取复杂查询仍用 Raw SQL（见 state.go），
// 模型只负责让单行/单表写走 ORM，并在换 MySQL 时集中处理列名与类型。

type messageRow struct {
	// ID 是自增主键，也是发给大模型看的 message_id：跨窗口、跨重启都不变。
	ID int64 `gorm:"column:id;primaryKey;autoIncrement"`
	// 三元组是原来的主键，现在降为唯一键：upsert 的冲突目标显式按它写，见 SaveMessage。
	WorkspaceID string `gorm:"column:workspace_id"`
	GroupID     string `gorm:"column:group_id"`
	MessageID   string `gorm:"column:message_id"`
	UserID      string `gorm:"column:user_id"`
	Content     string `gorm:"column:content"`
	Images      string `gorm:"column:images"`
	ImageIDs    string `gorm:"column:image_ids"`
	ReplyTo     string `gorm:"column:reply_to"`
	RefIdx      string `gorm:"column:ref_idx"`
	TS          int64  `gorm:"column:ts"`
}

func (messageRow) TableName() string { return "messages" }

// anchorRow 每个群「上次发出去的那个窗口」的首尾两条消息。
//
// 这两个位置是前缀缓存的命门：知道上次从哪条发到哪条，这次才能接着往后加，
// 前面那一大段才可能被服务端复用。改名带上 window_ 前缀是因为 start/last_sent
// 看不出说的是"窗口的起点和终点"，光看列名会以为是时间或者队列位置。
type anchorRow struct {
	WorkspaceID   string `gorm:"column:workspace_id;primaryKey"`
	GroupID       string `gorm:"column:group_id;primaryKey"`
	WindowFirstID string `gorm:"column:window_first_id"` // 上次窗口第一条消息的编号（messages.id）
	WindowLastID  string `gorm:"column:window_last_id"`  // 上次窗口最后一条消息的编号，之后的就是"新增"
	UpdatedAt     int64  `gorm:"column:updated_at"`      // 写入时刻（Unix 秒），同时用作缓存是否过期的基准
}

func (anchorRow) TableName() string { return "anchors" }

type identityRow struct {
	WorkspaceID string `gorm:"column:workspace_id;primaryKey"`
	GroupID     string `gorm:"column:group_id;primaryKey"`
	SelfUserID  string `gorm:"column:self_user_id"`
	LearnedAt   int64  `gorm:"column:learned_at"`
}

func (identityRow) TableName() string { return "identities" }

type memberRow struct {
	WorkspaceID string `gorm:"column:workspace_id;primaryKey"`
	GroupID     string `gorm:"column:group_id;primaryKey"`
	UserID      string `gorm:"column:user_id;primaryKey"`
	Name        string `gorm:"column:name"`
	DisplayName string `gorm:"column:display_name"`
	Extra       string `gorm:"column:extra"`
	UpdatedAt   int64  `gorm:"column:updated_at"`
}

func (memberRow) TableName() string { return "members" }

type groupRow struct {
	WorkspaceID string `gorm:"column:workspace_id;primaryKey"`
	GroupID     string `gorm:"column:group_id;primaryKey"`
	Name        string `gorm:"column:name"`
	Extra       string `gorm:"column:extra"`
	UpdatedAt   int64  `gorm:"column:updated_at"`
}

func (groupRow) TableName() string { return "groups" }

type groupPersonaRow struct {
	WorkspaceID string `gorm:"column:workspace_id;primaryKey"`
	GroupID     string `gorm:"column:group_id;primaryKey"`
	Keyword     string `gorm:"column:keyword"`
}

func (groupPersonaRow) TableName() string { return "group_personas" }

// WorkspaceProfile 是工作空间画像：常用字段建列，其余进 Extra JSON。
type WorkspaceProfile struct {
	WorkspaceID    string `gorm:"column:workspace_id;primaryKey"`
	Platform       string `gorm:"column:platform"`
	AccountKind    string `gorm:"column:account_kind"`
	Account        string `gorm:"column:account"`
	Name           string `gorm:"column:name"`
	Impl           string `gorm:"column:impl"`
	IdentityScheme string `gorm:"column:identity_scheme"`
	IdentityScope  string `gorm:"column:identity_scope"`
	Extra          string `gorm:"column:extra"`
	CreatedAt      int64  `gorm:"column:created_at"`
	UpdatedAt      int64  `gorm:"column:updated_at"`
}

func (WorkspaceProfile) TableName() string { return "workspace_profile" }

// MemberInfoHistory 成员信息变更历史。通用列只放常用查询字段，其余进 Extra。
type MemberInfoHistory struct {
	ID              int64  `gorm:"column:id;primaryKey;autoIncrement"`
	WorkspaceID     string `gorm:"column:workspace_id"`
	GroupID         string `gorm:"column:group_id"`
	UserID          string `gorm:"column:user_id"`
	Name            string `gorm:"column:name"`
	DisplayName     string `gorm:"column:display_name"`
	Source          string `gorm:"column:source"`
	SourceMessageID string `gorm:"column:source_message_id"`
	Extra           string `gorm:"column:extra"`
	ChangedAt       int64  `gorm:"column:changed_at"`
}

func (MemberInfoHistory) TableName() string { return "member_info_history" }

// GroupInfoHistory 群信息变更历史。
type GroupInfoHistory struct {
	ID              int64  `gorm:"column:id;primaryKey;autoIncrement"`
	WorkspaceID     string `gorm:"column:workspace_id"`
	GroupID         string `gorm:"column:group_id"`
	Name            string `gorm:"column:name"`
	Source          string `gorm:"column:source"`
	SourceMessageID string `gorm:"column:source_message_id"`
	Extra           string `gorm:"column:extra"`
	ChangedAt       int64  `gorm:"column:changed_at"`
}

func (GroupInfoHistory) TableName() string { return "group_info_history" }

// AssetTagJob 一条“某消息里的第几个资产”的补标签欠条。
type AssetTagJob struct {
	WorkspaceID string `gorm:"column:workspace_id;primaryKey"`
	GroupID     string `gorm:"column:group_id;primaryKey"`
	MessageID   string `gorm:"column:message_id;primaryKey"`
	AssetIndex  int    `gorm:"column:asset_index;primaryKey"`
	Kind        string `gorm:"column:kind"`
	URL         string `gorm:"column:url"`
	Status      string `gorm:"column:status"`
	Attempts    int    `gorm:"column:attempts"`
	LastError   string `gorm:"column:last_error"`
	CreatedAt   int64  `gorm:"column:created_at"`
	UpdatedAt   int64  `gorm:"column:updated_at"`
}

func (AssetTagJob) TableName() string { return "asset_tag_jobs" }

// SendRecord 发送台账的一行：动作编号是幂等键。
type SendRecord struct {
	WorkspaceID string `gorm:"column:workspace_id;primaryKey"`
	ActionID    string `gorm:"column:action_id;primaryKey"`
	MessageID   string `gorm:"column:message_id"`
	Text        string `gorm:"column:text"`
	Sent        bool   `gorm:"column:sent"`
	SendOK      bool   `gorm:"column:send_ok"`
	UpdatedAt   int64  `gorm:"column:updated_at"`
}

func (SendRecord) TableName() string { return "send_ledger" }

// Alert 放弃回复等异常现场。Detail 是完整 JSON。
type Alert struct {
	ID          int64  `gorm:"column:id;primaryKey;autoIncrement"`
	WorkspaceID string `gorm:"column:workspace_id"`
	Kind        string `gorm:"column:kind"`
	Detail      string `gorm:"column:detail"`
	CreatedAt   int64  `gorm:"column:created_at"`
}

func (Alert) TableName() string { return "alerts" }

// Setting 全局运行时设置。Key 是开放集合，所以保留 KV；画像那种固定字段不用它。
type Setting struct {
	Key       string `gorm:"column:key;primaryKey"`
	Value     string `gorm:"column:value"`
	UpdatedAt int64  `gorm:"column:updated_at"`
}

func (Setting) TableName() string { return "settings" }
