// Package app 是本程序的组合根（composition root）：唯一负责构造、启动与关闭全部运行时对象的地方。
//
// 依赖关系只在这里接线，因此除 main 包外不应 import 本包——组件需要别的组件时，
// 应当在 app 里注入，而不是让组件之间互相 import 或去读包级全局。
package app

import (
	"context"
	"fmt"
	"sync"
	"time"

	"good-review-master/bot"
	"good-review-master/cmd"
	"good-review-master/config"
	"good-review-master/internal/testutil"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/mcpclient"
	"good-review-master/mcpserver"
	"good-review-master/onebot"
	webserver "good-review-master/web/server"
)

// App 是全部运行时对象的聚合体：字段在 New 中一次性装配完成，之后只读。
type App struct {
	Config     *config.Config
	Prompt     *config.PromptConfig
	LLM        llm.Client
	FakeLLM    *testutil.FakeLLM // 仅测试模式非 nil
	OneBot     *onebot.Client
	MCP        *mcpclient.Manager
	BuiltinMCP *mcpserver.Server // 未启用看图（image_max<=0）或启动失败时为 nil
	Router     *cmd.Router
	Bot        *bot.Bot
	Web        *webserver.Server // web_port<=0 时为 nil

	ctx       context.Context // 生命周期 context：收到 SIGINT/SIGTERM 或 Shutdown 时取消
	stop      context.CancelFunc
	closeOnce sync.Once
}

// Ctx 返回应用生命周期 context。
func (a *App) Ctx() context.Context { return a.ctx }

// Start 启动消息轮询与 Web 管理面板，不阻塞。
// MCP 建连已在 New 中完成（内嵌看图的地址必须参与装配），故这里不再启动它。
func (a *App) Start() {
	go a.Bot.RunPollingLoop(a.ctx)

	if a.Web == nil {
		return
	}
	go func() {
		if err := a.Web.Start(); err != nil {
			logutil.Error("Web 服务异常退出", "err", err)
		}
	}()
	logutil.Info("Web 管理面板已启动", "addr", fmt.Sprintf("http://localhost:%d", a.Config.WebPort))
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

		if err := a.Router.Wait(); err != nil {
			logutil.Error("等待 goroutine 退出失败", "err", err)
		}

		// 关闭 MCP 会话（stdio 子进程随之终止）
		if err := a.MCP.Close(); err != nil {
			logutil.Error("关闭 MCP 服务失败", "err", err)
		}
		if a.BuiltinMCP != nil {
			if err := a.BuiltinMCP.Close(); err != nil {
				logutil.Warn("关闭内嵌 MCP 服务失败", "err", err)
			}
		}

		logutil.Info("已安全退出")
	})
}
