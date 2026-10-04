package store

import (
	"context"
	"time"

	"good-review-master/logutil"
	"good-review-master/telemetry"
)

// Informer 把"来源"与"快照"接起来，对标 client-go 的 SharedInformer：
//
//	List（首次/每次变更都重读）→ 整体替换快照 → 依次通知 handler
//
// 与 client-go 的差别同样源于文件源没有增量：那边是 DeltaFIFO + 增量索引，
// 这边每次变化就是一次完整 List。这也让语义简单很多——handler 拿到的
// old/new 一定是两份各自完整、自洽的配置。
type Informer[T any] struct {
	store    *Store[T]
	list     func() (*T, error)
	source   Source
	handlers []func(old, next *T)
	// resync 兜底全量重读的间隔（对应 client-go informer 的 resyncPeriod）。
	// 0 表示不启用。它的作用是防"变更信号漏了"——轮询指纹理论上会漏
	// （比如文件被改回原样再改回来），resync 保证最终一定收敛。
	resync time.Duration
}

// NewInformer 构造。list 必须无副作用且幂等——它会被反复调用。
func NewInformer[T any](target *Store[T], list func() (*T, error), source Source) *Informer[T] {
	return &Informer[T]{store: target, list: list, source: source}
}

// WithResync 设置兜底全量重读间隔（0 或负数表示不启用）。
func (i *Informer[T]) WithResync(interval time.Duration) *Informer[T] {
	i.resync = interval
	return i
}

// AddEventHandler 注册变更回调。Run 之前调用。
//
// 回调在 Informer 自己的 goroutine 里**串行**执行：
// 这样多个 handler 看到的是同一个推进顺序，不必各自加锁，
// 也避免了并发 Reload 同一个对象。代价是某个 handler 慢了会拖住后续——
// 本项目里的 handler 是"重读 YAML + 重建路由表"，都在毫秒级，可以接受。
func (i *Informer[T]) AddEventHandler(handler func(old, next *T)) {
	i.handlers = append(i.handlers, handler)
}

// Run 阻塞运行，直到 ctx 取消。
//
// 生命周期归调用方（app）：它返回前会一直占着一个 goroutine，
// 且**退不干净会拖住优雅关闭**，所以调用方必须保证 ctx 会取消。
func (i *Informer[T]) Run(ctx context.Context) error {
	changed := i.source.Changed(ctx)

	var resync <-chan time.Time
	if i.resync > 0 {
		ticker := time.NewTicker(i.resync)
		defer ticker.Stop()
		resync = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-changed:
			if !ok {
				// 来源自己关了，没有更多变更可等
				return nil
			}
			i.reload("auto")
		case <-resync:
			i.reload("resync")
		}
	}
}

// Reload 手动触发一次重读（对应 client-go 的 Resync()，也给"改完配置立刻生效"用）。
func (i *Informer[T]) Reload() {
	i.reload("manual")
}

// reload 重读一次并整体替换快照。
//
// 关键性质：**重读失败时保留旧快照**。用户改配置改到一半是常态，
// 这时宁可继续用上一份可用配置，也不能把系统推进"没有配置"的状态。
func (i *Informer[T]) reload(trigger string) {
	next, err := i.list()
	if err != nil {
		// 失败时沿用旧快照继续跑，服务本身毫无异样——没有这条计数器，
		// "配置改了但一直没生效"就只能靠人盯着日志发现。
		telemetry.ConfigReloadErrorsTotal.Inc()
		logutil.Warn("配置重读失败，继续沿用上一份快照", "触发方式", trigger, "err", err)
		return
	}
	previous := i.store.Get()
	i.store.replace(next)

	for _, handler := range i.handlers {
		i.callHandler(handler, previous, next)
	}
	telemetry.ConfigReloadTotal.WithLabelValues(trigger).Inc()
	telemetry.ConfigLastReloadTimestamp.SetToCurrentTime()
	logutil.Info("配置已重新加载", "触发方式", trigger)
}

// callHandler 隔一层 recover：一个 handler panic 不该把 Informer 的循环带走——
// 那样配置就再也不更新了，而且外面看不出来。
func (i *Informer[T]) callHandler(handler func(old, next *T), previous, next *T) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logutil.Error("配置变更回调 panic", "err", recovered)
		}
	}()
	handler(previous, next)
}
