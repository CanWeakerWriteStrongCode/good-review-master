package store

import (
	"context"
	"time"

	"good-review-master/logutil"
)

// Observer 只表达"来源变了，去处理一下"，**不持有快照**。
//
// 为什么需要它、而不复用 Informer：Informer 的职责是"我把最新快照替你存着"，
// 而这适用于**还没有快照的数据**。提示词配置不属于这一类——`config.PromptConfig`
// 自己就持有并原子发布了数据，外面再存一份就成了第二个事实来源，
// 迟早会出现"两个快照不一致、不知道该信哪个"的问题。
//
// 所以这里只保留两者真正共用的部分：Source（怎么知道变了）。
// 与 Informer 的循环有少量重复，但那是"两个形状不同的东西各写各的循环"，
// 比把它们硬压成一个带开关的抽象更好读。
type Observer struct {
	source   Source
	onChange func()
	// resync 兜底：即使变更信号漏了（轮询指纹理论上会漏），也定期重跑一次
	resync time.Duration
}

// NewObserver 构造。onChange 会被反复调用，必须幂等且无副作用。
func NewObserver(source Source, onChange func()) *Observer {
	return &Observer{source: source, onChange: onChange}
}

// WithResync 设置兜底重跑间隔（0 或负数表示不启用）。
func (o *Observer) WithResync(interval time.Duration) *Observer {
	o.resync = interval
	return o
}

// Run 阻塞运行直到 ctx 取消。生命周期归调用方，**它不退出会拖住优雅关闭**。
func (o *Observer) Run(ctx context.Context) error {
	changed := o.source.Changed(ctx)

	var resync <-chan time.Time
	if o.resync > 0 {
		ticker := time.NewTicker(o.resync)
		defer ticker.Stop()
		resync = ticker.C
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-changed:
			if !ok {
				return nil
			}
			o.notify("auto")
		case <-resync:
			o.notify("resync")
		}
	}
}

// notify 隔一层 recover：onChange 里要做若干事（重读文件、重建路由表），
// 一次 panic 不该把观察者带走——那样热更新就永远停了，而且外面看不出来。
func (o *Observer) notify(trigger string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logutil.Error("配置变更处理 panic", "触发方式", trigger, "err", recovered)
		}
	}()
	o.onChange()
}
