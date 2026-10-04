//go:build wireinject
// +build wireinject

package app

import (
	"good-review-master/bot"
	"good-review-master/config"
	"good-review-master/mcpclient"
	"good-review-master/router"
	"good-review-master/telemetry"

	"github.com/google/wire"
)

// initializeApp 是 Wire 的注入点：函数体里只写 wire.Build，实现由
// `go tool wire ./app` 生成到 wire_gen.go（生成物入库，没装 wire 的机器照样能编能跑）。
//
// # 这一行 wire.Build 就是整个依赖图
//
// 顺序由 Wire 按类型自己排，人只声明"谁由谁造"。比如它从
// provideOneBot → provideBotNickname → provideDerivedConfig 这条链看出
// "昵称必须在快照之前取到"——而那原本是代码注释里的一句话。
// 顺序从**阅读顺序**变成了**类型**，于是编译器能替你检查，这是引这套东西换到的主要收益。
//
// 校验：`go tool wire check ./app`。缺 provider 会在生成期报
// "no provider found for X"（不是等到运行期空指针）。
//
// # 这个文件为什么带 build tag
//
// 清单留在这里而不是 providers.go 的包级 var：那样 wire 包会被链进生产二进制，
// 还会多一次包级初始化。带 `wireinject` 标签的文件只在 wire 跑的时候参与构建，
// 所以 provider 函数与生产代码共用一个文件、图的定义却完全不进产物。
//
// # 改完 provider 一定要重跑
//
// 忘了重跑，编译器看到的仍是上一版 wire_gen.go：编译能过，行为停在旧图上。
// 判据是 `git status` 里 wire_gen.go 有没有该有的差异。
func initializeApp(inputs injectorInputs) (*App, error) {
	wire.Build(
		// 从 injector 输入里按字段取：FieldsOf 相当于"把结构体拆成若干个图节点"。
		// 拆开之后 llm.Client / context.Context 才能被 router.NewRouter 这样的构造函数直接用。
		wire.FieldsOf(new(injectorInputs), "Sources", "Loaded", "PromptPaths", "LLM", "Ctx"),
		// 从派生后的配置里取字段供下游使用（MCP 服务列表、指标监听地址）。
		// 注意写的是 new(*config.Config)：提供者给的是**指针**，
		// FieldsOf 的参数要写成指针的指针才表示"从指针上取字段"；
		// 写成 new(config.Config) 会让它去找一个没人提供的值类型。
		wire.FieldsOf(new(*config.Config), "MCPConfig", "MetricsAddr"),

		providePromptConfig,
		provideOneBot,
		provideBotNickname,
		provideBuiltinImageMCP,
		provideBuiltinImageAddr,
		provideDerivedConfig,
		provideConfigStore,
		provideConfigSnapshot,

		mcpclient.New,
		// 路由器只依赖 MCPProvider 这个窄接口，这里把唯一的实现接上去。
		// router 包因此完全不认识 mcpclient——方向是对的：能力由使用方定义。
		wire.Bind(new(router.MCPProvider), new(*mcpclient.Manager)),
		router.NewRouter,
		bot.NewBot,
		provideWebServer,

		telemetry.NewRegistry,
		provideMetricsServer,

		provideConfigInformer,
		providePromptObserver,
		provideApp,
	)
	return nil, nil
}
