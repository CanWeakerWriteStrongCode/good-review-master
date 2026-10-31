package bot

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"good-review-master/config"
	"good-review-master/onebot"
	"good-review-master/pipeline"
	"good-review-master/workspace"
)

// Bot 机器人运行时：IM 事件 → B1 → Stage 1 → B2 → Stage 2 → B3 → Stage 3。
//
// 每个阶段都是“每会话一个 worker、全局并发受限”的消费者（pipeline.Stage）；
// 同一群的顺序由分区键保证，不同群之间可以并行。
type Bot struct {
	cfg        config.Snapshot
	workspaces *workspace.Manager

	b1 pipeline.Buffer // 入站 → Stage 1
	b2 pipeline.Buffer // Stage 1 → Stage 2
	b3 pipeline.Buffer // Stage 2 → Stage 3

	stage1 *pipeline.Stage
	stage2 *pipeline.Stage
	stage3 *pipeline.Stage

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
		b1:         pipeline.NewMemoryBuffer(perChat, global),
		b2:         pipeline.NewMemoryBuffer(perChat, global),
		b3:         pipeline.NewMemoryBuffer(perChat, global),
	}
	b.stage1 = pipeline.NewStage(pipeline.StageOptions{
		Name: pipeline.StageSync, Buffer: b.b1, Config: b.syncConfig,
		Handler: b.handleSync, OnDeadLetter: b.deadLetter,
	})
	b.stage2 = pipeline.NewStage(pipeline.StageOptions{
		Name: pipeline.StageAssets, Buffer: b.b2, Config: b.assetConfig,
		Handler: b.handleAssets, OnDeadLetter: b.deadLetter,
	})
	b.stage3 = pipeline.NewStage(pipeline.StageOptions{
		Name: pipeline.StageAgent, Buffer: b.b3, Config: b.agentConfig,
		Handler: b.handleAgent, OnDeadLetter: b.deadLetter,
	})
	return b
}

func (b *Bot) syncConfig() pipeline.StageConfig {
	p := b.cfg().Pipeline
	return pipeline.StageConfig{
		Batch: p.SyncBatch, Linger: p.SyncLinger,
		Concurrency: p.SyncConcurrency, Retries: p.SyncRetries, IdleTTL: p.IdleTTL,
	}
}

func (b *Bot) assetConfig() pipeline.StageConfig {
	p := b.cfg().Pipeline
	return pipeline.StageConfig{
		Batch: p.AssetBatch, Linger: p.AssetLinger,
		Concurrency: p.AssetConcurrency, Retries: p.AssetRetries, IdleTTL: p.IdleTTL,
	}
}

func (b *Bot) agentConfig() pipeline.StageConfig {
	p := b.cfg().Pipeline
	return pipeline.StageConfig{
		Batch: p.AgentBatch, Linger: p.AgentLinger,
		Concurrency: p.AgentConcurrency, Retries: p.AgentRetries, IdleTTL: p.IdleTTL,
	}
}

// allowNicknameFallback 昵称兜底只在昵称唯一归属一个工作空间时可信。
func (b *Bot) allowNicknameFallback(ws *workspace.Workspace, nickname string) bool {
	if nickname == "" {
		return false
	}
	return len(b.workspaces.List()) == 1 || b.workspaces.NicknameUnique(nickname)
}

// eventTime 优先使用平台事件时间；缺失时退回本地时钟。
func eventTime(event onebot.Event) int64 {
	if event.Time > 0 {
		return int64(event.Time)
	}
	return time.Now().Unix()
}

// isAtBot 检查是否 @ 机器人（mention 段已由调用方先判）。
func isAtBot(content string, cfg *config.Config, allowNickname bool) bool {
	if allowNickname && cfg.BotNickname != "" && strings.HasPrefix(content, "@"+cfg.BotNickname) {
		return true
	}
	return strings.HasPrefix(content, "@你")
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
