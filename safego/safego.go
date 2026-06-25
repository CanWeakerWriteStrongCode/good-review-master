package safego

import (
	"context"
	"good-review-master/logutil"

	"golang.org/x/sync/errgroup"
)

// Group 安全 goroutine 管理器，封装 errgroup + panic recover
type Group struct {
	eg  *errgroup.Group
	ctx context.Context
}

// New 创建 Group，ctx 会被自动继承到每个 goroutine
func New(ctx context.Context) *Group {
	eg, egCtx := errgroup.WithContext(ctx)
	return &Group{eg: eg, ctx: egCtx}
}

// Go 安全启动 goroutine，ctx 自动传入，内置 panic recover
func (g *Group) Go(fn func(context.Context) error) {
	g.eg.Go(func() error {
		defer func() {
			if rc := recover(); rc != nil {
				logutil.Error("goroutine panic", "panic", rc)
			}
		}()
		return fn(g.ctx)
	})
}

// Wait 等待所有 goroutine 完成
func (g *Group) Wait() error {
	return g.eg.Wait()
}
