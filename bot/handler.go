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
// 现在是**一个前置 + 一条工作流**（机制仍是多条：添工作流仍从这里下手）：
//
//	聊天   前置补全 → 指令/闸门（command）→ 打标签（assets）→ 生成（agent）
//
// 指令是聊天工作流的第一站：它改的是后面两个域要不要跑（会话开关），
// 所以必须先执行、再让同一条消息走下去；其他指令执行完就到此为止。
// 「到此为止」还是「继续」写成指令自己的属性（Command.ContinueAfterRun），
// 拓扑只负责照抄——以后再加一条"执行完还要办事"的指令，只改那个字段，
// 不用在前置里加特例。
//
// 接 MQ 时：**工作流名就是消息的 tag，域的名字就是 topic**。消息带着 tag 在域之间
// 走，每个域只订阅自己的 topic，所以加一条工作流不建 topic、不复制消费组配置。
//
// 每个节点都是一个固定大小的协程池（pipeline.Consumer）：同一个会话在节点内串行，
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

// messageDedup 在入口做消息去重：sync 的缓存去重只能看到已经走完 agent 的消息，
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
	// 前置不放进工作流：它只做入口与补全，不属于任何一条。
	b.orch.mountRouter(pipeline.NodeSync, b.handleSync, b.syncConfig, perChat, global)
	b.orch.mount(pipeline.NodeAssets, b.handleAssets, b.assetConfig, perChat, global)
	b.orch.mount(pipeline.NodeAgent, b.handleAgent, b.agentConfig, perChat, global)
	b.orch.mount(pipeline.NodeCommand, b.handleCommand, b.commandConfig, perChat, global)

	// 工作流：前置路由到这里，指令先跑，再打标签、生成。
	//
	// 两条出口写在指令自己身上：门控指令（#启动）回 command_pass 继续往下，
	// 其余指令回 command_done 直接收尾——拓扑只照抄，不认关键字。
	// 顺序有意思：指令在打标签之前，所以 #启动 打开开关的那一刻，本条消息的
	// 打标签与生成已经能看到新状态（这也是它必须排在最前面的原因）。
	b.orch.workflow(WorkflowChat, pipeline.NodeCommand)
	b.orch.connect(WorkflowChat, EventCommandPass, pipeline.NodeAssets)
	b.orch.connect(WorkflowChat, EventTagged, pipeline.NodeAgent)
	b.orch.exit(WorkflowChat, EventCommandDone)
	b.orch.exit(WorkflowChat, EventReplied)
	return b
}

func (b *Bot) syncConfig() pipeline.ConsumerConfig {
	p := b.cfg().Pipeline
	return pipeline.ConsumerConfig{
		Batch: p.SyncBatch, Linger: p.SyncLinger,
		Concurrency: p.SyncConcurrency, Retries: p.SyncRetries,
	}
}

func (b *Bot) assetConfig() pipeline.ConsumerConfig {
	p := b.cfg().Pipeline
	return pipeline.ConsumerConfig{
		Batch: p.AssetBatch, Linger: p.AssetLinger,
		Concurrency: p.AssetConcurrency, Retries: p.AssetRetries,
	}
}

func (b *Bot) agentConfig() pipeline.ConsumerConfig {
	p := b.cfg().Pipeline
	return pipeline.ConsumerConfig{
		Batch: p.AgentBatch, Linger: p.AgentLinger,
		Concurrency: p.AgentConcurrency, Retries: p.AgentRetries,
	}
}

// commandConfig 管理指令域。Retries 固定 0：指令副作用不幂等，
// 重试一次就等于执行两次（#添加人格 会生成两个人格），所以不开放配置。
func (b *Bot) commandConfig() pipeline.ConsumerConfig {
	p := b.cfg().Pipeline
	return pipeline.ConsumerConfig{
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
