package bot

import (
	"sync"
	"sync/atomic"
	"time"

	"good-review-master/config"
	"good-review-master/pipeline"
	"good-review-master/workspace"
)

// Bot 机器人运行时，同时是这条流水线的**编排层**。
//
// 一条 IM 事件的走法：入口把消息放进前置 → 前置补名后决定进哪条工作流 →
// 工作流里的域依次处理，每一步都把结果交回编排层，由编排层按连线投给下一个域。
// 域与域之间没有引用关系，顺序写在 NewBot 的连线上。
//
// 现在是**一个前置 + 两条工作流**：
//
//	聊天   前置补名 → 打标签（assets）→ 生成（agent）
//	指令   前置补名 → 管理指令（command）
//
// 两条工作流在补名之后分开：控制平面不占打标签与生成的并发槽，也不进群聊缓存。
// 加一条工作流 = 一行 workflow + 几次 connect，前置不用动、也不需要新实例。
//
// 每个节点都是一个固定大小的协程池（pipeline.Stage）：同一个会话在节点内串行，
// 不同会话并行；会话闲着不占协程。
type Bot struct {
	cfg        config.Snapshot
	workspaces *workspace.Manager

	// orch 是编排层：域的挂载点、去向连线、起停与排空都在它手里。
	orch *orchestrator

	dedup *messageDedup

	closed    atomic.Bool
	closeOnce sync.Once
}

// messageDedup 在入口做消息去重：Stage 1 的缓存去重只能看到已经走完 Stage 3 的消息，
// 高并发补推时同一条消息可能在进入缓存前被处理两次。这里用 TTL 短表兜住这段窗口。
type messageDedup struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

const dedupTTL = 10 * time.Minute

func newMessageDedup() *messageDedup {
	return &messageDedup{seen: make(map[string]time.Time)}
}

// claim 返回 true 表示第一次见到这条消息。
func (d *messageDedup) claim(key string) bool {
	if d == nil || key == "" {
		return true
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	if at, ok := d.seen[key]; ok && now.Sub(at) < dedupTTL {
		return false
	}
	if len(d.seen) > 4096 {
		for item, at := range d.seen {
			if now.Sub(at) >= dedupTTL {
				delete(d.seen, item)
			}
		}
	}
	d.seen[key] = now
	return true
}

// forget 在入队失败时撤销占位，让实现端的重试能再次进入。
func (d *messageDedup) forget(key string) {
	if d == nil || key == "" {
		return
	}
	d.mu.Lock()
	delete(d.seen, key)
	d.mu.Unlock()
}

// NewBot 创建机器人实例。
func NewBot(cfg config.Snapshot, workspaces *workspace.Manager) *Bot {
	// 工作空间管理器持有叠加了 settings 表的快照（库 > 文件）。
	if workspaces != nil {
		cfg = workspaces.Snapshot()
	}
	current := cfg()
	perChat := current.Pipeline.PerChatCapacity
	global := current.Pipeline.GlobalCapacity
	// 直接构造 config.Config 的测试/嵌入式调用不会经过配置默认值，给安全兜底。
	if perChat <= 0 {
		perChat = 500
	}
	if global <= 0 {
		global = 5000
	}
	b := &Bot{
		cfg:        cfg,
		workspaces: workspaces,
		dedup:      newMessageDedup(),
	}
	b.orch = newOrchestrator(b.deadLetter)

	// 挂载节点。handler 只把结果交回来，不碰任何缓冲，所以这里不需要告诉它下游是谁。
	// 前置不放进工作流：它只回答“这条消息走哪条”，不属于任何一条。
	b.orch.mountRouter(pipeline.StageSync, b.handleIngest, b.syncConfig, perChat, global)
	b.orch.mount(pipeline.StageAssets, b.handleAssets, b.assetConfig, perChat, global)
	b.orch.mount(pipeline.StageAgent, b.handleAgent, b.agentConfig, perChat, global)
	b.orch.mount(pipeline.StageCommand, b.handleCommand, b.commandConfig, perChat, global)

	// 工作流一：聊天。前置路由到这里，之后打标签 → 生成。
	b.orch.workflow(WorkflowChat, pipeline.StageAssets)
	b.orch.connect(TopicTagged, pipeline.StageAgent)
	b.orch.exit(TopicReplied)

	// 工作流二：管理指令。只走控制平面，执行完就结束。
	//
	// 终点写在这里而不是让域“沉默”——域只汇报发生过什么，链决定还要不要往下走。
	// 将来要让指令执行完接下一个域，把这里换成 connect 即可，域一行代码都不用改。
	b.orch.workflow(WorkflowCommand, pipeline.StageCommand)
	b.orch.exit(TopicCommandDone)
	return b
}

func (b *Bot) syncConfig() pipeline.StageConfig {
	p := b.cfg().Pipeline
	return pipeline.StageConfig{
		Batch: p.SyncBatch, Linger: p.SyncLinger,
		Concurrency: p.SyncConcurrency, Retries: p.SyncRetries,
	}
}

func (b *Bot) assetConfig() pipeline.StageConfig {
	p := b.cfg().Pipeline
	return pipeline.StageConfig{
		Batch: p.AssetBatch, Linger: p.AssetLinger,
		Concurrency: p.AssetConcurrency, Retries: p.AssetRetries,
	}
}

func (b *Bot) agentConfig() pipeline.StageConfig {
	p := b.cfg().Pipeline
	return pipeline.StageConfig{
		Batch: p.AgentBatch, Linger: p.AgentLinger,
		Concurrency: p.AgentConcurrency, Retries: p.AgentRetries,
	}
}

// commandConfig 管理指令阶段。Retries 固定 0：指令副作用不幂等，
// 重试一次就等于执行两次（#添加人格 会生成两个人格），所以不开放配置。
func (b *Bot) commandConfig() pipeline.StageConfig {
	p := b.cfg().Pipeline
	return pipeline.StageConfig{
		Batch: p.CommandBatch, Linger: p.CommandLinger,
		Concurrency: p.CommandConcurrency, Retries: 0,
	}
}

// allowNicknameFallback 昵称兜底只在昵称唯一归属一个工作空间时可信。
// "@昵称" 算不算点名由 speech.IsAddressed 判断，这里只回答"能不能用昵称"。
func (b *Bot) allowNicknameFallback(ws *workspace.Workspace, nickname string) bool {
	if nickname == "" {
		return false
	}
	return len(b.workspaces.List()) == 1 || b.workspaces.NicknameUnique(nickname)
}

// truncate 按字符数截断（不是字节），避免把多字节字符切成乱码。
func truncate(text string, limit int) string {
	if limit <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
