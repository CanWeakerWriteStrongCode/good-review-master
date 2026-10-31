package pipeline

import (
	"context"
	"errors"
	"sync"
	"time"

	"good-review-master/logutil"
)

// StageConfig 是某个阶段当前生效的一批参数。每次取数/启动 worker 时现读，
// 这样 settings 表里改批量/并发/重试后不需要重启（容量除外：缓冲已分配）。
type StageConfig struct {
	Batch       int
	Linger      time.Duration
	Concurrency int
	Retries     int
	IdleTTL     time.Duration
}

// BatchHandler 处理一批 Item，返回需要重试的条目。已经处理成功的条目不要放回来。
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

// Stage 是“每会话一个 worker、全局并发受限”的阶段消费者。
//
// 同一 Key 只有一个 worker，严格按输入顺序处理；不同 Key 之间并行。
// worker 按需启动，空闲超过 IdleTTL 后自行退出（只回收处理者，不卸载任何数据）。
type Stage struct {
	opts StageOptions

	mu         sync.Mutex
	ctx        context.Context
	running    map[string]bool
	pending    []string
	pendingSet map[string]bool
	active     int
	wg         sync.WaitGroup
	closed     bool
}

func NewStage(opts StageOptions) *Stage {
	return &Stage{
		opts:       opts,
		ctx:        context.Background(),
		running:    make(map[string]bool),
		pendingSet: make(map[string]bool),
	}
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

// Run 记录生命周期 ctx，供 worker 使用。实际调度由 Submit/workerDone 驱动。
func (s *Stage) Run(ctx context.Context) {
	s.mu.Lock()
	s.ctx = ctx
	s.mu.Unlock()
}

// Submit 告诉阶段“这个分区有数据了”。没有空闲并发时先挂起，
// worker 退出时按队列顺序补位。Put 之后必须调用；重复调用是幂等的。
func (s *Stage) Submit(key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.running[key] {
		return
	}
	if s.active < s.Config().Concurrency {
		s.startLocked(key)
		return
	}
	if !s.pendingSet[key] {
		s.pendingSet[key] = true
		s.pending = append(s.pending, key)
	}
}

func (s *Stage) startLocked(key string) {
	s.running[key] = true
	s.active++
	s.wg.Add(1)
	go s.worker(key)
}

func (s *Stage) workerDone(key string) {
	s.mu.Lock()
	delete(s.running, key)
	s.active--
	if s.closed {
		s.mu.Unlock()
		s.wg.Done()
		return
	}
	concurrency := s.Config().Concurrency
	for len(s.pending) > 0 && s.active < concurrency {
		key := s.pending[0]
		s.pending = s.pending[1:]
		delete(s.pendingSet, key)
		if s.running[key] {
			continue
		}
		// 队列已经被另一个 worker 清空时不必再起一个空转 worker；
		// 新数据到达会再次 Submit。
		if s.opts.Buffer != nil && s.opts.Buffer.Len(key) == 0 {
			continue
		}
		s.startLocked(key)
	}
	s.mu.Unlock()
	s.wg.Done()
}

func (s *Stage) worker(key string) {
	defer s.workerDone(key)
	for {
		ctx := s.context()
		if ctx.Err() != nil {
			return
		}
		cfg := s.Config()
		items, err := s.take(ctx, key, cfg)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, ErrClosed) {
				return
			}
			if errors.Is(err, context.DeadlineExceeded) {
				// 空闲超时：回收这个会话的处理者。
				return
			}
			logutil.Warn("阶段取数失败", "stage", s.opts.Name, "key", key, "err", err)
			continue
		}
		if len(items) == 0 {
			continue
		}
		s.process(ctx, key, items, cfg.Retries)
	}
}

func (s *Stage) take(ctx context.Context, key string, cfg StageConfig) ([]Item, error) {
	if cfg.IdleTTL > 0 {
		idleCtx, cancel := context.WithTimeout(ctx, cfg.IdleTTL)
		defer cancel()
		items, err := s.opts.Buffer.Take(idleCtx, key, 1)
		if err != nil {
			return nil, err
		}
		return s.fillBatch(ctx, key, items, cfg), nil
	}
	items, err := s.opts.Buffer.Take(ctx, key, 1)
	if err != nil {
		return nil, err
	}
	return s.fillBatch(ctx, key, items, cfg), nil
}

func (s *Stage) fillBatch(ctx context.Context, key string, items []Item, cfg StageConfig) []Item {
	if len(items) == 0 {
		return items
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
		more, err := s.opts.Buffer.TakeReady(key, remaining)
		if err != nil {
			return items
		}
		items = append(items, more...)
	}
	return items
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

// Active 返回当前正在跑的会话 worker 数（指标与测试用）。
func (s *Stage) Active() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

// Wait 等待所有在跑的 worker 退出。调用前应先关闭上游缓冲。
func (s *Stage) Wait() {
	s.wg.Wait()
}

// Close 标记阶段关闭：不再启动新 worker；在跑的 worker 由缓冲 Close 唤醒。
func (s *Stage) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}
