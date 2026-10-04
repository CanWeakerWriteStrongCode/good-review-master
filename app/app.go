// Package app 是本程序的组合根（composition root）：唯一负责构造、启动与关闭全部运行时对象的地方。
//
// 依赖关系只在这里接线，因此除 main 包外不应 import 本包——组件需要别的组件时，
// 应当在 app 里注入，而不是让组件之间互相 import 或去读包级全局。
package app

import (
	"context"
	"sync"
	"time"

	"good-review-master/bot"
	"good-review-master/config"
	"good-review-master/config/store"
	"good-review-master/internal/testutil"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/mcpclient"
	"good-review-master/mcpserver"
	"good-review-master/onebot"
	"good-review-master/router"
	"good-review-master/telemetry"
	webserver "good-review-master/web/server"
)

// App 是全部运行时对象的聚合体：字段在 New 中一次性装配完成，之后只读。
type App struct {
	// Config 取当前配置快照。**每次用到时调一次，不要缓存返回值**（见 config.Snapshot 的约定）。
	Config     config.Snapshot
	Prompt     *config.PromptConfig
	LLM        llm.Client
	FakeLLM    *testutil.FakeLLM // 仅测试模式非 nil
	OneBot     *onebot.Client
	MCP        *mcpclient.Manager
	BuiltinMCP *mcpserver.Server // 未启用看图（image_max<=0）或启动失败时为 nil
	Router     *router.Router
	Bot        *bot.Bot
	Web        *webserver.Server        // web_port<=0 时为 nil
	Metrics    *telemetry.MetricsServer // metrics_addr=off 时为 nil

	ctx       context.Context // 生命周期 context：收到 SIGINT/SIGTERM 或 Shutdown 时取消
	stop      context.CancelFunc
	closeOnce sync.Once

	// 配置快照机制的内部件，以及"只有 app 知道"的派生值（见 build.go 的 derive）
	sources          config.Sources
	configStore      *store.Store[config.Config]
	configInformer   *store.Informer[config.Config]
	promptObserver   *store.Observer
	botNickname      string // 启动时从 NapCat 取到，之后不变
	builtinImageAddr string // 内嵌看图 MCP 的地址；空表示本会话没启用
	webURL           string // 启动时算好的面板地址，只用于启动日志

	// 可观测性：注册表只在这里持有（指标定义在 telemetry 包），
	// shutdownTrace 在追踪未启用时是一个 no-op 函数，永远非 nil。
	registry      *telemetry.Registry
	shutdownTrace func(context.Context) error
}

// Ctx 返回应用生命周期 context。
func (a *App) Ctx() context.Context { return a.ctx }

// Start 启动消息轮询、Web 管理面板与配置监听，不阻塞。
// MCP 建连已在 New 中完成（内嵌看图的地址必须参与装配），故这里不再启动它。
func (a *App) Start() {
	go a.Bot.RunPollingLoop(a.ctx)

	// 配置监听的 goroutine 必须挂在这个 ctx 上：它不退出就会拖住优雅关闭
	go func() {
		if err := a.configInformer.Run(a.ctx); err != nil {
			logutil.Error("配置监听退出", "err", err)
		}
	}()

	// 提示词监听同理：挂在同一个 ctx 上，退出时一起收
	go func() {
		if err := a.promptObserver.Run(a.ctx); err != nil {
			logutil.Error("提示词监听退出", "err", err)
		}
	}()

	// 指标端点：绑不上不该拖垮主流程（指标是附加能力），只记一条错误。
	// 真实场景：9100 被别的进程占了，此时机器人照常干活，只是没有指标可抓。
	if a.Metrics != nil {
		go func() {
			if err := a.Metrics.Start(); err != nil {
				logutil.Error("指标端点异常退出", "addr", a.Metrics.Addr(), "err", err)
			}
		}()
		logutil.Info("指标端点已启动", "addr", "http://"+a.Metrics.Addr()+"/metrics")
	}

	if a.Web == nil {
		return
	}
	go func() {
		if err := a.Web.Start(); err != nil {
			logutil.Error("Web 服务异常退出", "err", err)
		}
	}()
	logutil.Info("Web 管理面板已启动", "addr", a.webURL)
}

// Run 启动全部组件并阻塞，直到收到退出信号；随后完成优雅关闭。
// 关闭阶段的失败只记日志、不改变退出码，因此无返回值。
func (a *App) Run() {
	a.Start()
	<-a.ctx.Done()
	logutil.Info("收到退出信号，正在关闭...")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	a.Shutdown(ctx)
}

// Shutdown 按序优雅关闭，幂等（重复调用只执行一次）。
// 顺序与信号触发时一致：取消 ctx → Web → 路由 goroutine → MCP → 内嵌看图 MCP。
func (a *App) Shutdown(ctx context.Context) {
	a.closeOnce.Do(func() {
		// 先取消 ctx：停轮询、取消在途 LLM 调用，并释放信号监听
		a.stop()

		if a.Web != nil {
			if err := a.Web.Shutdown(ctx); err != nil {
				logutil.Error("Web 服务关闭失败", "err", err)
			}
		}

		if a.Metrics != nil {
			if err := a.Metrics.Shutdown(ctx); err != nil {
				logutil.Warn("指标端点关闭失败", "err", err)
			}
		}

		if err := a.Router.Wait(); err != nil {
			logutil.Error("等待 goroutine 退出失败", "err", err)
		}

		// 关闭 MCP 会话（stdio 子进程随之终止）。Close 目前恒返回 nil——
		// 子进程退出码只记 WARN，不算关闭失败（见 mcpclient.Manager.Close 的说明）。
		if err := a.MCP.Close(); err != nil {
			logutil.Error("关闭 MCP 服务失败", "err", err)
		}
		if a.BuiltinMCP != nil {
			if err := a.BuiltinMCP.Close(); err != nil {
				logutil.Warn("关闭内嵌 MCP 服务失败", "err", err)
			}
		}

		// 追踪放最后收：它要把批处理里还没上报的 span 冲刷出去，
		// 前面那些组件的关闭动作本身也可能产生 span。
		if a.shutdownTrace != nil {
			if err := a.shutdownTrace(ctx); err != nil {
				logutil.Warn("链路追踪关闭失败（末尾一批 span 可能丢失）", "err", err)
			}
		}

		logutil.Info("已安全退出")
	})
}
