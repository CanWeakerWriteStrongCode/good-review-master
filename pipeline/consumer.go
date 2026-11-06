package pipeline

import (
	"context"
	"sync"
	"time"

	"good-review-master/logutil"
	"good-review-master/telemetry"
)

// ConsumerConfig 是某个节点当前生效的一批参数。取数前现读，
// 所以 settings 表里改批量、等待、重试之后，下一批就生效。
//
// Concurrency 是例外：它是池子里协程的个数，只在 Run（进程启动）时读一次，
// 运行中改这个值不会动已有的池子，要重启。原因见 Run 的注释。
type ConsumerConfig struct {
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

// ConsumerOptions 构造一个节点的消费者。
type ConsumerOptions struct {
	Name         string
	Buffer       Buffer
	Config       func() ConsumerConfig
	Handler      BatchHandler
	OnDeadLetter func(item Item, err error)
}

// Consumer 是“固定协程池 + 每会话串行”的节点消费者。
//
// 池里的协程不绑定任何会话：它从就绪队列里挑一个 key 处理一批，交还之后再挑下一个。
// 所以协程数等于 Concurrency，与有多少个账号群无关；空闲的会话只是就绪队列里的一项，
// 一个协程也不占。
//
// 同一个 key 同一时刻只会有一个协程持有（inflight 标记），因此“同一个账号在同一个群里
// 按到达顺序串行处理”这条不变；不同 key 之间并行。每个节点各有一个 Consumer，
// 名额互不挤占，但一条消息仍然要依次走完它所在工作流的那几个节点。
//
// 池子的协程数在 Run 时定死，运行中不扩缩：改 *_concurrency 要重启。
type Consumer struct {
	opts   ConsumerOptions
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

// NewConsumer 构造一个节点的消费者。
//
// Buffer 必须实现 Peeker：Consumer 的调度模型是「拉 + 回头看一眼」——
// 派活前要确认这个会话真有积压（next）、处理完要看还有没有剩下的（release）、
// 取数要先非阻塞探一条再补齐（takeBatch）。少了“看得见队列”这个能力，
// “处理完还有剩”就无法发现，消息会静静留在队列里没人取。
//
// MQ 的推送模型不是做不到，而是它的 Peeker 语义变成“本地预取队列里有多少”，
// 不是 broker 全量。真要接 MQ，要么给消费端维护本地预取队列并实现 Peeker，
// 要么按 MQ 自己的消费循环另写一套调度——不要假装它是内存队列。
func NewConsumer(opts ConsumerOptions) *Consumer {
	if opts.Buffer == nil {
		panic("pipeline: Consumer 需要一个 Buffer")
	}
	peeker, ok := opts.Buffer.(Peeker)
	if !ok {
		panic("pipeline: Consumer 的 Buffer 必须实现 Peeker（要能按分区数积压、非阻塞取数），" +
			"否则“处理完还有没有剩下的”发现不了，消息会滞留在队列里")
	}
	w := &Consumer{
		opts:     opts,
		peeker:   peeker,
		ctx:      context.Background(),
		readySet: make(map[string]bool),
		inflight: make(map[string]bool),
	}
	w.cond = sync.NewCond(&w.mu)
	return w
}

func (w *Consumer) Config() ConsumerConfig {
	if w.opts.Config == nil {
		return ConsumerConfig{Batch: 1, Concurrency: 1}
	}
	cfg := w.opts.Config()
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

// Run 按当时的并发数把池拉起来。这是池子唯一的起点，之后不再扩缩。
//
// 不让 Submit 顺手起协程，是因为它可能在 Run 之前就被调用（测试里直接投一批、
// 或者 Run 之前先放一条）：那时 w.ctx 还是 context.Background()，拉起来的协程
// 既不跟随关停，也看不到真正的生命周期。池子的起点只留 Run 一个。
func (w *Consumer) Run(ctx context.Context) {
	cfg := w.Config()
	w.mu.Lock()
	w.ctx = ctx
	w.startLocked(cfg.Concurrency)
	w.mu.Unlock()
	w.observe()
}

// Submit 告诉消费者“这个分区有数据了”。它只登记到就绪队列，不启动任何协程；
// 池里哪个协程空出来，谁就去取。Put 之后必须调用，重复调用是幂等的。
func (w *Consumer) Submit(key string) {
	if key == "" {
		return
	}
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	if w.inflight[key] {
		// 这个会话正在被处理。处理完那一批协程会自己回头看积压，
		// 在途期间到达的消息不会漏。
		w.mu.Unlock()
		w.observe()
		return
	}
	w.markReadyLocked(key)
	w.cond.Signal()
	w.mu.Unlock()
	w.observe()
}

func (w *Consumer) markReadyLocked(key string) {
	if w.readySet[key] {
		return
	}
	w.readySet[key] = true
	w.ready = append(w.ready, key)
}

// startLocked 按 target 起池子。只在 Run 里调一次，之后不扩也不缩，
// 所以这里没有「多了就让超编的协程退场」那一半——退场只发生在关闭时。
func (w *Consumer) startLocked(target int) {
	if target < 1 {
		target = 1 // 兜底：并发配成 0 会让流水线停摆
	}
	for w.workers < target {
		w.workers++
		w.wg.Add(1)
		go w.worker()
	}
}

// next 阻塞到拿到一个有数据的分区，返回时该 key 已标记为处理中。
// 第二个返回值 false 表示这个协程该退场了（消费者关闭）；计数在这里扣，
// 保证判断和减数在同一把锁里，不会几个协程一起退光了。
func (w *Consumer) next() (string, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for {
		if w.closed || w.ctx.Err() != nil {
			w.workers--
			return "", false
		}
		for len(w.ready) > 0 {
			key := w.ready[0]
			w.ready = w.ready[1:]
			if !w.readySet[key] {
				continue
			}
			delete(w.readySet, key)
			if w.inflight[key] {
				continue
			}
			// 队列已经被处理完的分区不必再派活，新数据到达会重新 Submit。
			if w.peeker.Len(key) == 0 {
				continue
			}
			w.inflight[key] = true
			return key, true
		}
		w.cond.Wait()
	}
}

func (w *Consumer) worker() {
	defer w.wg.Done()
	for {
		key, ok := w.next()
		if !ok {
			return
		}
		// 先刷一次，让 active 在批次开始的瞬间就 +1，
		// 不然 agent 跑 40 秒的那段时间里指标还停在旧值。
		w.observe()
		w.drain(key)
		w.release(key)
	}
}

// drain 连续处理同一个会话的积压，最多 maxBurstPerTurn 批就让位。
// 让位的目的是公平：活多的会话不会一直在池子里转，别的会话能插进来。
func (w *Consumer) drain(key string) {
	ctx := w.context()
	for burst := 0; burst < maxBurstPerTurn && ctx.Err() == nil; burst++ {
		cfg := w.Config()
		items := w.takeBatch(ctx, key, cfg)
		if len(items) == 0 {
			return
		}
		start := time.Now()
		w.process(ctx, key, items, cfg.Retries)
		telemetry.PipelineNodeBatchDurationSeconds.WithLabelValues(w.opts.Name).
			Observe(time.Since(start).Seconds())
		w.observe()
	}
}

// takeBatch 从分区取一批。有活才会被派过来，所以这里是非阻塞取：
// 队列空就返回 nil，不去等（等数据是就绪队列的事，不该占着协程）。
func (w *Consumer) takeBatch(ctx context.Context, key string, cfg ConsumerConfig) []Item {
	items, err := w.peeker.TakeReady(key, 1)
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
		if more, err := w.peeker.TakeReady(key, remaining); err == nil {
			items = append(items, more...)
		}
	}
	return items
}

// release 交还分区：还有积压就重新挂回就绪队列（排到队尾，让别的会话先来），
// 然后把协程还给池子。
func (w *Consumer) release(key string) {
	w.mu.Lock()
	delete(w.inflight, key)
	if w.closed {
		w.mu.Unlock()
		return
	}
	if w.peeker.Len(key) > 0 {
		w.markReadyLocked(key)
		w.cond.Signal()
	}
	w.mu.Unlock()
	w.observe()
}

// observe 把当前状态刷到指标上。必须在锁外调用：Buffer.Total() 要拿缓冲层自己的锁。
// 频度是“每批一次”，对几毫秒到几十秒的处理耗时可忽略不计。
func (w *Consumer) observe() {
	node := w.opts.Name
	w.mu.Lock()
	workers, active, ready := w.workers, len(w.inflight), len(w.ready)
	w.mu.Unlock()
	telemetry.PipelineNodeWorkers.WithLabelValues(node).Set(float64(workers))
	telemetry.PipelineNodeActive.WithLabelValues(node).Set(float64(active))
	telemetry.PipelineNodeReady.WithLabelValues(node).Set(float64(ready))
	if w.opts.Buffer != nil {
		telemetry.PipelineNodeBacklog.WithLabelValues(node).Set(float64(w.opts.Buffer.Total()))
	}
}

func (w *Consumer) process(ctx context.Context, key string, items []Item, retries int) {
	retryItems := w.opts.Handler(ctx, key, items)
	for attempt := 1; len(retryItems) > 0; attempt++ {
		if attempt > retries || ctx.Err() != nil {
			for _, failed := range retryItems {
				if w.opts.OnDeadLetter != nil {
					w.opts.OnDeadLetter(failed.Item, failed.Err)
				} else {
					logutil.Error("节点处理失败，超过重试上限",
						"node", w.opts.Name, "item", failed.Item.ID, "err", failed.Err)
				}
			}
			return
		}
		next := make([]FailedItem, 0, len(retryItems))
		for _, failed := range retryItems {
			failed.Item.Attempts++
			next = append(next, w.opts.Handler(ctx, key, []Item{failed.Item})...)
		}
		retryItems = next
	}
}

func (w *Consumer) context() context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ctx
}

// Active 返回当前正在处理批次的会话数（指标与测试用）。
// 空闲会话不计入，池里睡着的协程也不计入。
func (w *Consumer) Active() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.inflight)
}

// Workers 返回当前池里的协程数（指标与测试用）。
func (w *Consumer) Workers() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.workers
}

// Wait 等待池里所有协程退出。调用前应先关闭上游缓冲并调用 Close。
func (w *Consumer) Wait() {
	w.wg.Wait()
}

// Close 标记消费者关闭并唤醒池里所有等活的协程。正在处理的那一批会跑完。
func (w *Consumer) Close() {
	w.mu.Lock()
	w.closed = true
	w.cond.Broadcast()
	w.mu.Unlock()
}
