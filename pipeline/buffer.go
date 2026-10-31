package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrClosed 缓冲已关闭。入站层据此把事件当作不可接收处理。
var ErrClosed = errors.New("pipeline: buffer closed")

// Item 是缓冲区里的最小单位。Body 是 JSON，保证换成 MQ 时不用改业务代码。
type Item struct {
	ID       string // 幂等键：stage + key + 消息 ID
	Key      string // 分区键：M0 是 serial，M4 起是 workspaceID|chatID
	Stage    string // ingest / sync / assets / agent
	Body     []byte // JSON 编码的阶段载荷
	Attempts int
}

// Buffer 是阶段之间的传送带。Put 放、Take 批量取、Ack 确认、Close 关闭。
//
// 内存实现的 Ack 是空操作（Take 即出队）；接 MQ 后 Ack 对应提交位点。
type Buffer interface {
	Put(ctx context.Context, item Item) error
	Take(ctx context.Context, key string, max int) ([]Item, error)
	// TakeReady 非阻塞取数：没有数据时返回空切片，用于凑批。
	TakeReady(key string, max int) ([]Item, error)
	Ack(ctx context.Context, items []Item) error
	Close() error
	// Len 返回某个分区当前积压条数（worker 退出与重启判断用）。
	Len(key string) int
	// Total 返回所有分区积压总数（关停排空用）。
	Total() int
}

// MemoryBuffer 进程内 FIFO：每个 Key 一条队列，另有一道全局限量。
//
// Put 在满时等待（由 ctx 控制超时），绝不静默丢消息；Close 后 Put 返回 ErrClosed，
// Take 会先排空已有项，再返回 ErrClosed。
type MemoryBuffer struct {
	mu     sync.Mutex
	queues map[string][]Item
	total  int
	perKey int
	global int
	closed bool

	// data/space 是广播通道：每次放/取都关闭旧通道再换新的，所有等待者都会被唤醒。
	// 用单槽 channel 通知会在多分区时丢唤醒——唤醒被另一个分区的消费者抢走后，
	// 真正有数据的分区会一直等下去。
	data  chan struct{}
	space chan struct{}
	done  chan struct{}
	once  sync.Once
}

// NewMemoryBuffer 创建内存缓冲。perKey/global <=0 时实例化为 1，避免无容量队列。
func NewMemoryBuffer(perKey, global int) *MemoryBuffer {
	if perKey <= 0 {
		perKey = 1
	}
	if global <= 0 {
		global = perKey
	}
	return &MemoryBuffer{
		queues: make(map[string][]Item),
		perKey: perKey,
		global: global,
		data:   make(chan struct{}, 1),
		space:  make(chan struct{}, 1),
		done:   make(chan struct{}),
	}
}

// Put 入队；容量满时阻塞到腾出空间、ctx 取消或缓冲关闭。
func (b *MemoryBuffer) Put(ctx context.Context, item Item) error {
	if item.Key == "" {
		item.Key = SerialKey
	}
	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return ErrClosed
		}
		q := b.queues[item.Key]
		if len(q) < b.perKey && b.total < b.global {
			b.queues[item.Key] = append(q, item)
			b.total++
			broadcast(&b.data)
			b.mu.Unlock()
			return nil
		}
		wait := b.space
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return fmt.Errorf("消息队列已满（容量 %d）：%w", b.global, ctx.Err())
		case <-wait:
		case <-b.done:
			return ErrClosed
		}
	}
}

// Take 从指定分区取最多 max 条；没有数据时阻塞到有数据、ctx 取消或关闭。
// 取出的项即出队；Ack 在内存实现里是空操作。
func (b *MemoryBuffer) Take(ctx context.Context, key string, max int) ([]Item, error) {
	if key == "" {
		key = SerialKey
	}
	if max <= 0 {
		max = 1
	}
	for {
		b.mu.Lock()
		q := b.queues[key]
		if len(q) > 0 {
			n := min(max, len(q))
			out := append([]Item(nil), q[:n]...)
			if n == len(q) {
				delete(b.queues, key)
			} else {
				b.queues[key] = q[n:]
			}
			b.total -= n
			broadcast(&b.space)
			b.mu.Unlock()
			return out, nil
		}
		closed := b.closed
		wait := b.data
		b.mu.Unlock()
		if closed {
			return nil, ErrClosed
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wait:
		case <-b.done:
			// 关闭后先排空；下一轮循环会返回 ErrClosed。
		}
	}
}

// TakeReady 非阻塞取数；队列为空或已关闭时返回空切片。
func (b *MemoryBuffer) TakeReady(key string, max int) ([]Item, error) {
	if key == "" {
		key = SerialKey
	}
	if max <= 0 {
		max = 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	q := b.queues[key]
	if len(q) == 0 {
		return nil, nil
	}
	n := min(max, len(q))
	out := append([]Item(nil), q[:n]...)
	if n == len(q) {
		delete(b.queues, key)
	} else {
		b.queues[key] = q[n:]
	}
	b.total -= n
	broadcast(&b.space)
	return out, nil
}

// Ack 在内存实现里是空操作：Take 已经把项移出队列。
func (b *MemoryBuffer) Ack(context.Context, []Item) error { return nil }

// Close 停止接收新项。已被阻塞的 Put/Take 会被唤醒。
func (b *MemoryBuffer) Close() error {
	b.once.Do(func() {
		b.mu.Lock()
		b.closed = true
		b.mu.Unlock()
		close(b.done)
	})
	return nil
}

// Len 返回某个分区当前的项数（测试与指标用）。
func (b *MemoryBuffer) Len(key string) int {
	if key == "" {
		key = SerialKey
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.queues[key])
}

// Total 返回所有分区的总项数（测试与指标用）。
func (b *MemoryBuffer) Total() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.total
}

// broadcast 关闭当前通道并换上新的：等待者拿的是旧通道，会全部被唤醒。
// 必须在持有 b.mu 时调用。
func broadcast(ch *chan struct{}) {
	close(*ch)
	*ch = make(chan struct{})
}
