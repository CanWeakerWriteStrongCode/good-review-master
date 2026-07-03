package async

import (
	"context"

	"good-review-master/logutil"
	"good-review-master/pool"
)

// Group 安全 goroutine 管理器，封装协程池 + panic recover
type Group struct {
	pool   *pool.Pool
	ctx    context.Context
	cancel context.CancelFunc
}

// New 创建 Group，ctx 会被自动继承到每个 goroutine
func New(ctx context.Context) *Group {
	ctx, cancel := context.WithCancel(ctx)
	return &Group{
		pool:   pool.New(0), // 0 = 使用默认大小
		ctx:    ctx,
		cancel: cancel,
	}
}

// Go 安全提交任务，ctx 自动传入，内置 panic recover
func (g *Group) Go(fn func(context.Context) error) {
	// 快速路径：已取消则丢弃
	select {
	case <-g.ctx.Done():
		return
	default:
	}

	task := func() {
		defer func() {
			if rc := recover(); rc != nil {
				logutil.Error("goroutine panic", "panic", rc)
			}
		}()
		_ = fn(g.ctx) // 错误忽略，与原行为一致
	}

	// 阻塞提交：等待有空闲 worker 或上下文取消
	for !g.pool.Submit(task) {
		select {
		case <-g.ctx.Done():
			return
		default:
			// 队列满，自旋重试
		}
	}
}

// Wait 等待所有 goroutine 完成
func (g *Group) Wait() error {
	g.cancel()        // 阻止新任务提交
	g.pool.Shutdown() // 等待所有 worker 完成
	return nil
}
