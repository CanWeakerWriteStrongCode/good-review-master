package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
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

// New 装配整个应用：加载配置、构造各组件并接线，返回可直接 Run 的 App。
//
// 所有可能失败的步骤（配置、校验、大模型提供商）都排在占用任何资源之前，
// 因此 New 失败时调用方无需清理。
// 注意 New 并非无副作用的纯构造：内嵌看图 MCP 要在此绑定端口（地址得参与装配），
// MCP 建连也在这里发起。
func New(opts Options) (*App, error) {
	sources := config.Sources{Config: opts.ConfigPath, Secret: opts.SecretPath}

	// 配置的合法性（含"开了 web_port 就必须有账号密码"）由 config.Load 内部的
	// 域内校验 + Config.Validate 一次性判定，New 不再自己重复检查一遍。
	cfg, err := config.Load(sources)
	if err != nil {
		return nil, fmt.Errorf("加载配置失败：%w", err)
	}

	promptCfg, err := config.LoadPromptConfig(opts.SystemPromptPath, config.CustomPromptPath(opts.SystemPromptPath))
	if err != nil {
		return nil, fmt.Errorf("加载提示词配置失败：%w", err)
	}

	application := &App{Prompt: promptCfg, sources: sources}

	// 大模型客户端（测试模式用 FakeLLM，不走真实 API）
	if opts.TestMode {
		application.FakeLLM = testutil.NewFakeLLM()
		application.LLM = application.FakeLLM
		logutil.Warn("测试模式已启用：使用 FakeLLM，NapCat 指向死地址")
	} else {
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
			// 参数取的是一次启动快照——熔断器/限速器是有状态的，热更新数值
			// 会让计数含义漂移，不值得。
			application.LLM = llm.NewResilient(adapter, llm.ResilientOptions{
				RatePerSecond:   cfg.LLMConfig.RateLimitPerSec,
				RateLimitBurst:  cfg.LLMConfig.RateLimitBurst,
				BreakerFailures: cfg.LLMConfig.BreakerFailures,
				BreakerCooldown: cfg.LLMConfig.BreakerCooldown,
			})
		default:
			// 理论上到不了这里：provider 白名单已在 llmSection.Validate 里拦下。
			// 留着是为了让"加了 provider 却没加分支"立刻编译期/启动期可见，而不是静默走错。
			return nil, fmt.Errorf("不支持的大模型提供商：%s（config 校验通过却无对应分支，请检查 app/build.go）",
				cfg.LLMConfig.Provider)
		}
	}

	// shutdown context：MCP 会话、路由 goroutine、消息轮询、配置监听都挂在它下面
	application.ctx, application.stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	application.OneBot = onebot.NewClient(cfg.NapCatHTTPAPI, cfg.NapCatAccessToken)

	// 昵称必须在建快照**之前**取到：快照要求不可变，"先建快照再往里面写昵称"
	// 正是这次改造要消掉的那种运行期写入。
	if info, err := application.OneBot.GetLoginInfo(); err != nil {
		logutil.Warn("获取机器人昵称失败，@检测仅使用QQ号", "err", err)
	} else {
		application.botNickname = info.Nickname
		logutil.Info("机器人昵称", "nickname", application.botNickname)
	}

	application.setupBuiltinImageMCP(cfg)

	// 第一份快照：贴上昵称与内嵌看图服务后发布出去，之后就不再修改它
	initial := application.derive(cfg)
	application.configStore = store.NewStore(initial)
	application.Config = application.configStore.Get // 方法值，类型恰为 config.Snapshot
	application.webURL = fmt.Sprintf("http://localhost:%d", initial.WebPort)

	// 可观测性：指标注册表与（可选）链路追踪都在这里建好，Shutdown 时一起收。
	// 用 initial 而不是每次读快照：metrics_addr / otlp_endpoint 都是启动期决定
	// （监听器已绑定、导出器已构造），改了要重启——warnRestartOnlyChanges 会提示。
	application.registry = telemetry.NewRegistry()
	shutdownTrace, err := telemetry.InitTracing(initial.OTLPEndpoint, "good-review")
	switch {
	case err != nil:
		// 追踪配错了不该拦住启动：没有链路不影响机器人干活
		logutil.Warn("链路追踪初始化失败，本次运行不采集链路", "err", err)
	case initial.OTLPEndpoint != "":
		// 只在真的开了的时候说一声：这条日志是"我配的 endpoint 到底生效没有"的唯一直接证据
		logutil.Info("链路追踪已启用", "endpoint", initial.OTLPEndpoint)
	}
	application.shutdownTrace = shutdownTrace
	if initial.MetricsAddr != config.MetricsAddrOff {
		application.Metrics = telemetry.NewMetricsServer(initial.MetricsAddr, application.registry)
	}

	// MCP 工具服务：后台并发连接每个服务并自动拉取工具清单（tools/list），
	// 拉到后原子更新快照，之后每次对话自动把 inject 服务的工具作为 function calling 注入。
	// 不阻塞启动：连不上的服务由重连循环按 retry_interval_sec 接管。
	// 注意用的是 initial 而非快照：MCP 的服务列表**不**随配置热更新（见阶段 7）。
	application.MCP = mcpclient.New(initial.MCPConfig, application.ctx)
	application.MCP.LogConfig()
	application.MCP.Start()

	application.Router = router.NewRouter(application.Config, promptCfg, application.LLM, application.OneBot, application.MCP, application.ctx)

	logutil.Info("🚀 【不是好评大师】机器人启动成功")
	logutil.Info("机器人QQ：" + initial.BotQQ)
	logutil.Info("允许响应群：" + initial.AllowGroupsStr())
	logutil.Info("NapCat HTTP API：" + initial.NapCatHTTPAPI)

	application.Bot = bot.NewBot(application.Config, application.OneBot, application.Router)

	if initial.WebPort > 0 {
		application.Web = webserver.New(application.Config, application.OneBot)
		if opts.TestMode {
			application.Web.EnableDebug(application.Router, application.FakeLLM)
		}
	}

	// 配置监听：把"重新加载"这件事收敛到一个入口。
	// list 里走的是 derive 而**不是**裸的 config.Load——否则每次重读磁盘配置
	// 都会把昵称和内嵌看图服务从快照里抹掉（它们不在文件里）。
	application.configInformer = store.NewInformer(application.configStore, func() (*config.Config, error) {
		reloaded, err := config.Load(sources)
		if err != nil {
			return nil, err
		}
		return application.derive(reloaded), nil
	}, store.NewFilePoller(configWatchInterval, opts.ConfigPath, opts.SecretPath))
	application.configInformer.AddEventHandler(warnRestartOnlyChanges)

	// 提示词监听：提示词有自己的合并语义（系统只读 + 自定义每次读盘），
	// 数据也由 PromptConfig 自己持有，所以这里只观察"变了"，不另存一份快照。
	application.promptObserver = store.NewObserver(
		store.NewFilePoller(configWatchInterval,
			opts.SystemPromptPath, config.CustomPromptPath(opts.SystemPromptPath)),
		application.reloadPrompts,
	)

	return application, nil
}

// reloadPrompts 提示词文件变化后的动作：重读并重建路由表。
//
// 两步缺一不可：Reload 只换数据，而路由表（前缀树 + 帮助列表）是从数据**派生**出来的，
// 不重建的话新关键字根本命中不了——用户会看到"配置文件里明明写了，发出去却没反应"。
func (a *App) reloadPrompts() {
	a.Prompt.Reload()
	a.Router.Rebuild()
	data := a.Prompt.Snapshot()
	logutil.Info("提示词已热更新并重建路由表", "指令类别数", len(data.CmdConfigs))
}

// warnRestartOnlyChanges 报告"改了但不会热生效"的配置项。
//
// 为什么需要这个告警：热更新一上线，用户会理所当然地以为改什么都能立刻生效。
// 而下面这几项在启动时就固化了——端口已绑定、路由表与鉴权中间件已按当时的值装好、
// MCP 客户端已按当时的服务列表构造。不提示的话，用户会以为改动生效了，
// 然后花时间排查"为什么改端口没用"。
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

// derive 把"只有 app 知道"的运行期值贴到一份刚加载出来的配置上，得到可以发布的快照。
//
// 这些值不在任何文件里（昵称来自 NapCat，地址来自本次进程绑定的端口），
// 但它们又必须出现在快照里，否则 bot 的 @检测 与 MCP 的工具注入就看不到。
// 所以**每一次重新加载都要经过这里**，而不是只有启动时走一次。
//
// 本函数不改动入参之外的东西，也不打日志（重载会反复调用，日志会刷屏）。
func (a *App) derive(base *config.Config) *config.Config {
	base.BotNickname = a.botNickname

	if a.builtinImageAddr == "" {
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
		URL:       a.builtinImageAddr,
		Token:     base.McpBuiltinToken,
	})
	base.MCPConfig.Servers = servers
	return base
}

// setupBuiltinImageMCP 在 llm.image_max>0 时启动内嵌「看图」MCP 服务（提供 view_image），
// 并把它的地址记在 App 上，由 derive 注入到每一份配置快照里。
//
// 只监听 127.0.0.1；mcp_builtin_token 非空则服务端校验 Bearer，客户端带同一 token。
// 启动失败不致命：只记日志，本会话没有 view_image 工具。
func (a *App) setupBuiltinImageMCP(cfg *config.Config) {
	if cfg.LLMConfig.ImageMax <= 0 {
		return
	}

	builtinServer := mcpserver.New(nil, cfg.McpBuiltinToken)
	addr, err := builtinServer.Start()
	if err != nil {
		logutil.Error("内嵌 MCP 服务(看图)启动失败，本会话无 view_image 工具", "err", err)
		return
	}

	if !cfg.MCPConfig.Enabled {
		logutil.Warn("启用看图(lm image_max>0)自动开启 mcp.enabled，注入内嵌 view_image 服务")
	}

	a.BuiltinMCP = builtinServer
	a.builtinImageAddr = addr
	logutil.Info("内嵌 MCP 服务(看图)已启动", "url", addr, "带Bearer鉴权", cfg.McpBuiltinToken != "")
}
