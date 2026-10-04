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

// GoCtx 在**调用方 context** 的基础上提交任务。
//
// 和 Go 的区别只有一处，但很关键：Go 只把 Group 自己的生命周期 ctx 传进去，
// 于是任务与"是谁触发的"彻底断开了联系——链路追踪走到这里就断链，
// 调用方的超时也传不进来。GoCtx 让任务 ctx 同时继承两者：
// 以 parent 为父（带上 trace span、调用方超时），并随 Group 关闭而取消。
//
// 两条取消路径缺一不可：只继承 parent 会让任务在进程关闭时继续跑，
// 只继承 Group 又丢掉了调用方的上下文。
func (g *Group) GoCtx(parent context.Context, fn func(context.Context) error) {
	ctx, cancel := context.WithCancel(parent)
	// Group 取消时立刻连带取消任务。AfterFunc 返回的 stop 用来在任务正常结束后
	// 摘掉这个回调，避免 g.ctx 上堆积已经没用的回调。
	stop := context.AfterFunc(g.ctx, cancel)

	g.Go(func(context.Context) error {
		defer stop()
		defer cancel()
		return fn(ctx)
	})
}

// Wait 等待所有 goroutine 完成
func (g *Group) Wait() error {
	g.cancel()        // 阻止新任务提交
	g.pool.Shutdown() // 等待所有 worker 完成
	return nil
}
