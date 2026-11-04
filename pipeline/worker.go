package pipeline

import (
	"context"
	"sync"
	"time"

	"good-review-master/logutil"
	"good-review-master/telemetry"
)

// StageConfig 是某个阶段当前生效的一批参数。取数前现读，
// 这样 settings 表里改批量/并发/重试后不需要重启。
type StageConfig struct {
	Batch       int
	Linger      time.Duration
	Concurrency int
	Retries     int
}

// maxBurstPerTurn 一个协程一次最多连续处理同一个会话的几批。
// 吃完就让位，避免活多的会话长期霸着池子把别的会话饿死。
const maxBurstPerTurn = 3

// BatchHandler 处理一批 Item，返回需要重试的条目。已经处理成功的条目不要放回来。
//
// 一批长什么样由传输层负责保证，域可以依赖三条：
//  1. items 非空；
//  2. 一批内所有 Item 的 Key 相同——跨 key 的批要先分组再调；
//  3. 顺序即到达顺序，同一个 key 不会有两个协程并发处理。
//
// 换流式传输（MQ）时，这三条由适配层补齐：先攒批、再按 Key 分组，然后照这个契约调进来。
type BatchHandler func(ctx context.Context, key string, items []Item) []FailedItem

// FailedItem 一条处理失败的 Item 及原因。
type FailedItem struct {
	Item Item
	Err  error
}

// StageOptions 构造一个阶段消费者。
type StageOptions struct {
	Name         string
	Buffer       Buffer
	Config       func() StageConfig
	Handler      BatchHandler
	OnDeadLetter func(item Item, err error)
}

// Stage 是“固定协程池 + 每会话串行”的阶段消费者。
//
// 池里的协程不绑定任何会话：它从就绪队列里挑一个 key 处理一批，交还之后再挑下一个。
// 所以协程数等于 Concurrency，与有多少个账号群无关；空闲的会话只是就绪队列里的一项，
// 一个协程也不占。
//
// 同一个 key 同一时刻只会有一个协程持有（inflight 标记），因此“同一个账号在同一个群里
// 按到达顺序串行处理”这条不变；不同 key 之间并行。不同阶段各有一个 Stage，
// 名额互不挤占，但一条消息仍然要依次走完三个阶段。
type Stage struct {
	opts   StageOptions
	peeker Peeker // 构造期就断言好，取数路径不必每批再断言

	mu       sync.Mutex
	cond     *sync.Cond
	ctx      context.Context
	ready    []string        // 就绪队列：队首是最久没被处理的 key
	readySet map[string]bool // ready 的去重索引
	inflight map[string]bool // 正被某个协程持有的 key
	workers  int             // 存活的池协程数
	wg       sync.WaitGroup
	closed   bool
}

// NewStage 构造一个阶段消费者。
//
// Buffer 必须实现 Peeker：Stage 的调度模型是「拉 + 回头看一眼」——
// 派活前要确认这个会话真有积压（next）、处理完要看还有没有剩下的（release）、
// 取数要先非阻塞探一条再补齐（takeBatch）。少了“看得见队列”这个能力，
// “处理完还有剩”就无法发现，消息会静静留在队列里没人取。
//
// MQ 的推送模型不是做不到，而是它的 Peeker 语义变成“本地预取队列里有多少”，
// 不是 broker 全量。真要接 MQ，要么给消费端维护本地预取队列并实现 Peeker，
// 要么按 MQ 自己的消费循环另写一套调度——不要假装它是内存队列。
func NewStage(opts StageOptions) *Stage {
	if opts.Buffer == nil {
		panic("pipeline: Stage 需要一个 Buffer")
	}
	peeker, ok := opts.Buffer.(Peeker)
	if !ok {
		panic("pipeline: Stage 的 Buffer 必须实现 Peeker（要能按分区数积压、非阻塞取数），" +
			"否则“处理完还有没有剩下的”发现不了，消息会滞留在队列里")
	}
	s := &Stage{
		opts:     opts,
		peeker:   peeker,
		ctx:      context.Background(),
		readySet: make(map[string]bool),
		inflight: make(map[string]bool),
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

func (s *Stage) Config() StageConfig {
	if s.opts.Config == nil {
		return StageConfig{Batch: 1, Concurrency: 1}
	}
	cfg := s.opts.Config()
	if cfg.Batch <= 0 {
		cfg.Batch = 1
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 1
	}
	if cfg.Retries < 0 {
		cfg.Retries = 0
	}
	return cfg
}

// Run 按当时的并发数把池拉起来。之后并发数变化会在 Submit/交还时自动扩缩。
func (s *Stage) Run(ctx context.Context) {
	cfg := s.Config()
	s.mu.Lock()
	s.ctx = ctx
	s.resizeLocked(cfg.Concurrency)
	s.mu.Unlock()
	s.observe()
}

// Submit 告诉阶段“这个分区有数据了”。它只登记到就绪队列，不启动任何协程；
// 池里哪个协程空出来，谁就去取。Put 之后必须调用，重复调用是幂等的。
func (s *Stage) Submit(key string) {
	if key == "" {
		return
	}
	cfg := s.Config()
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.resizeLocked(cfg.Concurrency)
	if s.inflight[key] {
		// 这个会话正在被处理。处理完那一批协程会自己回头看积压，
		// 在途期间到达的消息不会漏。
		s.mu.Unlock()
		s.observe()
		return
	}
	s.markReadyLocked(key)
	s.cond.Signal()
	s.mu.Unlock()
	s.observe()
}

func (s *Stage) markReadyLocked(key string) {
	if s.readySet[key] {
		return
	}
	s.readySet[key] = true
	s.ready = append(s.ready, key)
}

// resizeLocked 把协程数调整到 target。少就补，多就广播一次让超编的协程在 next 里自行退场。
func (s *Stage) resizeLocked(target int) {
	if target < 1 {
		target = 1
	}
	for s.workers < target {
		s.workers++
		s.wg.Add(1)
		go s.worker()
	}
	if s.workers > target {
		s.cond.Broadcast()
	}
}

// next 阻塞到拿到一个有数据的分区，返回时该 key 已标记为处理中。
// 第二个返回值 false 表示这个协程该退场了（阶段关闭或并发被调小）；
// 计数在这里扣，保证判断和减数在同一把锁里，不会几个协程一起退光了。
func (s *Stage) next() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.closed || s.ctx.Err() != nil {
			s.workers--
			return "", false
		}
		if s.workers-1 >= s.Config().Concurrency {
			s.workers--
			return "", false
		}
		for len(s.ready) > 0 {
			key := s.ready[0]
			s.ready = s.ready[1:]
			if !s.readySet[key] {
				continue
			}
			delete(s.readySet, key)
			if s.inflight[key] {
				continue
			}
			// 队列已经被处理完的分区不必再派活，新数据到达会重新 Submit。
			if s.peeker.Len(key) == 0 {
				continue
			}
			s.inflight[key] = true
			return key, true
		}
		s.cond.Wait()
	}
}

func (s *Stage) worker() {
	defer s.wg.Done()
	for {
		key, ok := s.next()
		if !ok {
			return
		}
		// 先刷一次，让 active 在批次开始的瞬间就 +1，
		// 不然 Stage 3 跑 40 秒的那段时间里指标还停在旧值。
		s.observe()
		s.drain(key)
		s.release(key)
	}
}

// drain 连续处理同一个会话的积压，最多 maxBurstPerTurn 批就让位。
// 让位的目的是公平：活多的会话不会一直在池子里转，别的会话能插进来。
func (s *Stage) drain(key string) {
	ctx := s.context()
	for burst := 0; burst < maxBurstPerTurn && ctx.Err() == nil; burst++ {
		cfg := s.Config()
		items := s.takeBatch(ctx, key, cfg)
		if len(items) == 0 {
			return
		}
		start := time.Now()
		s.process(ctx, key, items, cfg.Retries)
		telemetry.PipelineStageBatchDurationSeconds.WithLabelValues(s.opts.Name).
			Observe(time.Since(start).Seconds())
		s.observe()
	}
}

// takeBatch 从分区取一批。有活才会被派过来，所以这里是非阻塞取：
// 队列空就返回 nil，不去等（等数据是就绪队列的事，不该占着协程）。
func (s *Stage) takeBatch(ctx context.Context, key string, cfg StageConfig) []Item {
	items, err := s.peeker.TakeReady(key, 1)
	if err != nil || len(items) == 0 {
		return nil
	}
	if cfg.Linger > 0 {
		timer := time.NewTimer(cfg.Linger)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
	if remaining := cfg.Batch - len(items); remaining > 0 {
		if more, err := s.peeker.TakeReady(key, remaining); err == nil {
			items = append(items, more...)
		}
	}
	return items
}

// release 交还分区：还有积压就重新挂回就绪队列（排到队尾，让别的会话先来），
// 然后把协程还给池子。
func (s *Stage) release(key string) {
	cfg := s.Config()
	s.mu.Lock()
	delete(s.inflight, key)
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.peeker.Len(key) > 0 {
		s.markReadyLocked(key)
		s.cond.Signal()
	}
	s.resizeLocked(cfg.Concurrency)
	s.mu.Unlock()
	s.observe()
}

// observe 把当前状态刷到指标上。必须在锁外调用：Buffer.Total() 要拿缓冲层自己的锁。
// 频度是“每批一次”，对几毫秒到几十秒的处理耗时可忽略不计。
func (s *Stage) observe() {
	stage := s.opts.Name
	s.mu.Lock()
	workers, active, ready := s.workers, len(s.inflight), len(s.ready)
	s.mu.Unlock()
	telemetry.PipelineStageWorkers.WithLabelValues(stage).Set(float64(workers))
	telemetry.PipelineStageActive.WithLabelValues(stage).Set(float64(active))
	telemetry.PipelineStageReady.WithLabelValues(stage).Set(float64(ready))
	if s.opts.Buffer != nil {
		telemetry.PipelineStageBacklog.WithLabelValues(stage).Set(float64(s.opts.Buffer.Total()))
	}
}

func (s *Stage) process(ctx context.Context, key string, items []Item, retries int) {
	retryItems := s.opts.Handler(ctx, key, items)
	for attempt := 1; len(retryItems) > 0; attempt++ {
		if attempt > retries || ctx.Err() != nil {
			for _, failed := range retryItems {
				if s.opts.OnDeadLetter != nil {
					s.opts.OnDeadLetter(failed.Item, failed.Err)
				} else {
					logutil.Error("阶段处理失败，超过重试上限",
						"stage", s.opts.Name, "item", failed.Item.ID, "err", failed.Err)
				}
			}
			return
		}
		next := make([]FailedItem, 0, len(retryItems))
		for _, failed := range retryItems {
			failed.Item.Attempts++
			next = append(next, s.opts.Handler(ctx, key, []Item{failed.Item})...)
		}
		retryItems = next
	}
}

func (s *Stage) context() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx
}

// Active 返回当前正在处理批次的会话数（指标与测试用）。
// 空闲会话不计入，池里睡着的协程也不计入。
func (s *Stage) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.inflight)
}

// Workers 返回当前池里的协程数（指标与测试用）。
func (s *Stage) Workers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workers
}

// Wait 等待池里所有协程退出。调用前应先关闭上游缓冲并调用 Close。
func (s *Stage) Wait() {
	s.wg.Wait()
}

// Close 标记阶段关闭并唤醒池里所有等活的协程。正在处理的那一批会跑完。
func (s *Stage) Close() {
	s.mu.Lock()
	s.closed = true
	s.cond.Broadcast()
	s.mu.Unlock()
}
