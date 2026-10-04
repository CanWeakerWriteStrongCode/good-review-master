package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"good-review-master/config"
	"good-review-master/internal/testutil"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/router"
	"good-review-master/telemetry"
)

// Options 装配输入（都是外部环境决定、组件自己拿不到的东西）。
type Options struct {
	ConfigPath       string // config.yaml 路径
	SecretPath       string // secret.yaml 路径（可以不存在）
	SystemPromptPath string // prompt_system.yaml 路径
	TestMode         bool   // true：用 FakeLLM 顶替真实大模型，并开放 /api/debug/* 自测接口
}

// configWatchInterval 配置文件轮询间隔。
// 2 秒是"改完几乎立刻生效"与"别把磁盘刷穿"之间的折中；文件源只能轮询，
// 每次 tick 只做几次 os.Stat，开销可忽略。
const configWatchInterval = 2 * time.Second

// New 装配整个应用。
//
// **依赖图本身不在这里**——它声明在 providers.go，由 `go tool wire ./app` 生成
// wire_gen.go。本函数只做 Wire 表达不了的那部分：凡带副作用、或顺序本身即语义的动作。
//
// 具体是四类：
//
//  1. 生命周期：signal.NotifyContext 要在任何组件跑起来之前装好，
//     而且它注册的是进程级信号处理器，不是"某个依赖"；
//  2. 按运行期条件二选一：大模型实现由 GOOD_REVIEW_TEST 决定，
//     而 Wire 的图是编译期定死的，不能"看环境变量选实现"；
//  3. 首跑与日志：main 负责建模板文件，这里负责把加载结果打成启动日志；
//  4. 启动动作的时序：MCP 的 LogConfig/Start 与面板的 EnableDebug 都是
//     构造之后的**方法调用**，Wire 只接构造函数。
//
// 顺序上有一处硬约束：**昵称必须在快照构造之前取到**（见 providers.go 的
// provideBotNickname）——快照要求不可变，"先发布再往里面写昵称"正是这次重构要消掉的动作。
func New(opts Options) (*App, error) {
	// 配置只读这一次。**必须在造大模型客户端之前**：客户端的 provider 分支、
	// 接口地址、采样参数全都来自它。校验失败就在这里返回，
	// 此时还没占任何资源（信号处理器、端口、子进程都还没动）。
	sources := config.Sources{Config: opts.ConfigPath, Secret: opts.SecretPath}
	loaded, err := config.Load(sources)
	if err != nil {
		return nil, fmt.Errorf("加载配置失败：%w", err)
	}

	llmClient, fakeLLM, err := buildLLMClient(opts, loaded)
	if err != nil {
		return nil, err
	}

	// 生命周期 context：MCP 会话、路由 goroutine、消息轮询、配置监听都挂在它下面。
	// 它属于 App 的"状态"而不是"依赖"，所以不进依赖图，构造完再赋值。
	lifecycleCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	application, err := initializeApp(injectorInputs{
		Sources: sources,
		Loaded:  loadedConfig{loaded},
		PromptPaths: promptPaths{
			System: opts.SystemPromptPath,
			Custom: config.CustomPromptPath(opts.SystemPromptPath),
		},
		LLM: llmClient,
		Ctx: lifecycleCtx,
	})
	if err != nil {
		// 装配失败时还没起任何后台东西，但信号处理器已经装上了，要收掉
		stop()
		return nil, err
	}

	application.ctx = lifecycleCtx
	application.stop = stop
	application.FakeLLM = fakeLLM

	// 可观测性：注册表随依赖图一起建好了，追踪是全局状态（otel.SetTracerProvider），
	// 带副作用且失败要降级，所以留在这一层。
	shutdownTrace, traceErr := telemetry.InitTracing(loaded.OTLPEndpoint, "good-review")
	switch {
	case traceErr != nil:
		// 追踪配错了不该拦住启动：没有链路不影响机器人干活
		logutil.Warn("链路追踪初始化失败，本次运行不采集链路", "err", traceErr)
	case loaded.OTLPEndpoint != "":
		// 只在真的开了的时候说一声：这条日志是"我配的 endpoint 到底生效没有"的唯一直接证据
		logutil.Info("链路追踪已启用", "endpoint", loaded.OTLPEndpoint)
	}
	application.shutdownTrace = shutdownTrace

	// MCP 工具服务：构造已经在图里完成，这里触发"连上去 + 拉工具清单"。
	// 不阻塞启动：连不上的服务由重连循环按 retry_interval_sec 接管。
	// 顺序上排在启动摘要之前，是为了让日志读起来仍是"组件先自报家门，再打总结"。
	application.MCP.LogConfig()
	application.MCP.Start()

	logStartup(application)

	if application.Web != nil && fakeLLM != nil {
		application.Web.EnableDebug(application.Router, fakeLLM)
	}

	return application, nil
}

// buildLLMClient 按运行环境挑大模型实现。
//
// 这一步必须在依赖图之外：Wire 的图是编译期定死的，而"测试模式用 FakeLLM"
// 是运行期条件。所以选好的实现作为 injector 的**入参**传进去，
// 图里那一堆下游（router / 测试接口）看到的就是同一个 llm.Client。
//
// 同时返回具体的 FakeLLM：只有测试模式非 nil，Web 面板要用它注册 /api/debug/*。
func buildLLMClient(opts Options, cfg *config.Config) (llm.Client, *testutil.FakeLLM, error) {
	if opts.TestMode {
		fakeLLM := testutil.NewFakeLLM()
		logutil.Warn("测试模式已启用：使用 FakeLLM，NapCat 指向死地址")
		return fakeLLM, fakeLLM, nil
	}

	switch cfg.LLMConfig.Provider {
	case "openai":
		adapter := llm.NewOpenAIAdapter(
			cfg.LLMConfig.APIKey,
			cfg.LLMConfig.APIBase,
			cfg.LLMConfig.ModelName,
			cfg.LLMConfig.Temperature,
			cfg.LLMConfig.TopP,
		)
		// 出站保护（限速 + 熔断）包在适配器外面，而不是塞进适配器内部：
		// 它管的是"要不要发这一趟"，与"怎么发"是两件事，分开后各自可测。
		return llm.NewResilient(adapter, llm.ResilientOptions{
			RatePerSecond:   cfg.LLMConfig.RateLimitPerSec,
			RateLimitBurst:  cfg.LLMConfig.RateLimitBurst,
			BreakerFailures: cfg.LLMConfig.BreakerFailures,
			BreakerCooldown: cfg.LLMConfig.BreakerCooldown,
		}), nil, nil
	default:
		// 理论上到不了这里：provider 白名单已在 llmSection.Validate 里拦下。
		// 留着是为了让"加了 provider 却没加分支"立刻编译期/启动期可见，而不是静默走错。
		return nil, nil, fmt.Errorf("不支持的大模型提供商：%s（config 校验通过却无对应分支，请检查 app/build.go）",
			cfg.LLMConfig.Provider)
	}
}

// logStartup 打启动摘要。只有这里知道"哪些东西真的起来了"（面板可能被禁用、
// 指标端点可能被关掉），所以它读的是装配结果而不是配置。
func logStartup(application *App) {
	cfg := application.Config()
	logutil.Info("🚀 【不是好评大师】机器人启动成功")
	logutil.Info("机器人QQ：" + cfg.BotQQ)
	logutil.Info("允许响应群：" + cfg.AllowGroupsStr())
	logutil.Info("NapCat HTTP API：" + cfg.NapCatHTTPAPI)
}

// reloadPrompts 提示词文件变化后的动作：重读并重建路由表。
//
// 两步缺一不可：Reload 只换数据，而路由表（前缀树 + 帮助列表）是从数据**派生**出来的，
// 不重建的话新关键字根本命中不了——用户会看到"配置文件里明明写了，发出去却没反应"。
//
// 写成自由函数而不是 App 的方法：监听器的 provider 需要它，而 App 需要监听器——
// 方法值会把这个依赖变成环（App → observer → App）。
func reloadPrompts(promptCfg *config.PromptConfig, messageRouter *router.Router) {
	promptCfg.Reload()
	messageRouter.Rebuild()
	logutil.Info("提示词已热更新并重建路由表", "指令类别数", len(promptCfg.Snapshot().CmdConfigs))
}

// warnRestartOnlyChanges 报告"改了但不会热生效"的配置项。
//
// 为什么需要这个告警：热更新一上线，用户会理所当然地以为改什么都能立刻生效。
// 而下面这几项在启动时就固化了——端口已绑定、路由表与鉴权中间件已按当时的值装好、
// MCP 客户端已按当时的服务列表构造、限速器与熔断器是有状态的。
// 不提示的话，用户会以为改动生效了，然后花时间排查"为什么改端口没用"。
func warnRestartOnlyChanges(previous, next *config.Config) {
	if previous.WebPort != next.WebPort {
		logutil.Warn("web_port 已变化，但需要重启才能生效（监听器在启动时已绑定）",
			"原值", previous.WebPort, "新值", next.WebPort)
	}
	if previous.WebUsername != next.WebUsername || previous.WebPassword != next.WebPassword {
		logutil.Warn("Web 登录账号或密码已变化，但需要重启才能生效（鉴权中间件在启动时已装好）")
	}
	if previous.JWTSecret != next.JWTSecret {
		logutil.Warn("jwt_secret 已变化，但需要重启才能生效（已签发的 token 仍按旧密钥校验）")
	}
	if !sameMCPServers(previous.MCPConfig.Servers, next.MCPConfig.Servers) {
		logutil.Warn("mcp.servers 已变化，但需要重启才能生效（MCP 客户端在启动时已按当时的列表构造）")
	}
	if previous.MetricsAddr != next.MetricsAddr {
		logutil.Warn("metrics_addr 已变化，但需要重启才能生效（监听器在启动时已绑定）",
			"原值", previous.MetricsAddr, "新值", next.MetricsAddr)
	}
	if previous.OTLPEndpoint != next.OTLPEndpoint {
		logutil.Warn("otlp_endpoint 已变化，但需要重启才能生效（追踪导出器在启动时已构造）",
			"原值", previous.OTLPEndpoint, "新值", next.OTLPEndpoint)
	}
	// 限速器与熔断器是**有状态**的（令牌桶、连续失败计数），中途换数值会让计数含义漂移，
	// 所以它们和监听端口一样属于启动期决定。用户同样会以为改了就生效，照样要提示。
	if previous.LLMConfig.RateLimitPerSec != next.LLMConfig.RateLimitPerSec ||
		previous.LLMConfig.RateLimitBurst != next.LLMConfig.RateLimitBurst ||
		previous.LLMConfig.BreakerFailures != next.LLMConfig.BreakerFailures ||
		previous.LLMConfig.BreakerCooldown != next.LLMConfig.BreakerCooldown {
		logutil.Warn("llm 的限速或熔断参数已变化，但需要重启才能生效（限速器与熔断器在启动时已构造）")
	}
	if previous.MCPConfig.BreakerFailures != next.MCPConfig.BreakerFailures ||
		previous.MCPConfig.BreakerCooldown != next.MCPConfig.BreakerCooldown {
		logutil.Warn("mcp 的熔断参数已变化，但需要重启才能生效（熔断器在启动时已按服务构造）")
	}
}

// sameMCPServers 比较两份服务列表是否等价。逐个字段比，不用 reflect.DeepEqual：
// 后者会把 nil 与空切片判为不等，从而对同一份配置误报变化。
func sameMCPServers(previous, next []config.MCPServerConf) bool {
	if len(previous) != len(next) {
		return false
	}
	for i := range previous {
		left, right := previous[i], next[i]
		if left.Name != right.Name || left.Transport != right.Transport ||
			left.URL != right.URL || left.Token != right.Token ||
			left.Command != right.Command || left.ShouldInject() != right.ShouldInject() ||
			strings.Join(left.Args, "\x00") != strings.Join(right.Args, "\x00") {
			return false
		}
	}
	return true
}

// deriveConfig 把"只有本进程知道"的运行期值贴到一份刚加载出来的配置上，
// 得到可以发布的快照内容。
//
// 这些值不在任何文件里（昵称来自 NapCat，地址来自本次进程绑定的端口），
// 但它们又必须出现在快照里，否则 bot 的 @检测 与 MCP 的工具注入就看不到。
// 所以**每一次重新加载都要经过这里**，而不是只有启动时走一次。
//
// 本函数不改动入参之外的东西，也不打日志（重载会反复调用，日志会刷屏）。
//
// 写成自由函数而不是 App 的方法：配置监听器的 provider 需要它，
// 而 App 需要监听器——方法值会把依赖变成环。
func deriveConfig(base *config.Config, nickname botNickname, addr builtinImageAddr) *config.Config {
	base.BotNickname = string(nickname)

	if addr == "" {
		return base
	}
	base.MCPConfig.Enabled = true
	// 复制一份再 append：直接 append 到切片的底层数组上，
	// 会让这份配置与"已发布出去的旧快照"共享底层数组——旧的必须保持不可变
	servers := make([]config.MCPServerConf, 0, len(base.MCPConfig.Servers)+1)
	servers = append(servers, base.MCPConfig.Servers...)
	servers = append(servers, config.MCPServerConf{
		Name:      "builtin_image",
		Transport: "http",
		URL:       string(addr),
		Token:     base.McpBuiltinToken,
	})
	base.MCPConfig.Servers = servers
	return base
}
