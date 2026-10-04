package app

import (
	"context"
	"fmt"

	"good-review-master/bot"
	"good-review-master/config"
	"good-review-master/config/store"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/mcpclient"
	"good-review-master/mcpserver"
	"good-review-master/onebot"
	"good-review-master/router"
	"good-review-master/telemetry"
	webserver "good-review-master/web/server"
)

// 本文件是组合根的 provider 声明：**每个节点只说"我由什么造出来"**，
// 顺序交给 Wire 在编译期排（wire_gen.go 是生成物，不进人手维护）。
//
// # 分界线
//
// Wire 只接**构造函数**。凡带副作用的编排一律留在 build.go：
// signal.NotifyContext、首跑建配置文件、按 testMode 挑大模型实现、
// 启动日志、Start/Shutdown 的调用顺序。那些东西没有"依赖"可言，只有"什么时候做"，
// 塞进 provider 只会把副作用藏进一个看起来像纯构造的地方。
//
// # 一条必须认下来的代价
//
// **Wire 按类型装配。** 同一个类型出现两次（两个 string、两份 *config.Config），
// 它就分不清谁是谁。所以下面这些"同一份数据的两个阶段/两个同类型的值"
// 必须各自变成不同的类型——这不是为了好看，是装配本身的要求：
//
//   - loadedConfig：刚从文件读出来、还没贴派生值的配置（derive 需要昵称，
//     而昵称又来自这份配置里的 NapCat 地址；两者若是同一个节点，依赖就成环了）
//   - botNickname / builtinImageAddr：两个字面量都是 string，但一个是昵称、一个是地址
//
// 反过来说，这就是 Wire 在这类项目上的真实收益与真实成本：它把"谁在谁之前"
// 从**阅读顺序**变成了**类型**，于是编译器能替你检查；代价是凭空多出几个包装类型。

// loadedConfig 刚从文件加载出来、还没贴派生值的配置。
type loadedConfig struct{ *config.Config }

// botNickname 机器人昵称（启动时从 NapCat 取到，之后不变）。
type botNickname string

// builtinImageAddr 内嵌看图 MCP 的监听地址；空表示本会话没启用。
type builtinImageAddr string

// promptPaths 两个提示词文件的路径。
// 用结构体而不是两个 string 参数，理由同上：Wire 分不清两个 string。
type promptPaths struct {
	System string
	Custom string
}

// injectorInputs 是 Wire 拿不到、只能由 build.go 提供的东西。
//
// 打成结构体而不是散在 injector 参数表里：以后再加一项输入不用改 injector 签名，
// 而 injector 的签名是生成物 wire_gen.go 的入口，改它等于每次都要重新生成。
type injectorInputs struct {
	// Sources 既用于首轮加载（build.go 里做），也用于**重载**：
	// 配置监听器每次收到"文件变了"都要照这两个路径重读一遍。
	Sources config.Sources
	// Loaded 是 build.go 读出来的那一份原始配置。放在输入里而不是图里加载，
	// 是因为 build.go 需要它先造大模型客户端（按 provider 分支 + testMode 二选一），
	// 而 Wire 的图是编译期定死的、表达不了这种运行期条件。
	Loaded      loadedConfig
	PromptPaths promptPaths
	LLM         llm.Client
	Ctx         context.Context
}

// providerSet 是唯一的 provider 清单。
//
// 新增组件时改这里 + 重跑 `go tool wire ./app`。忘了重跑的话编译能过、
// 但跑的还是旧图——所以改完 provider 一定要看 `git status` 里有没有 wire_gen.go 的差异。
//
// **provider 清单在 wire.go**，刻意住在带 build tag 的文件里：那样
// `github.com/google/wire` 这个包不会被链进生产二进制，包级 var 初始化也少一次
// （wire.NewSet/Bind/FieldsOf 在运行期都是空操作，这个仓库的约定是启动期零副作用）。

// providePromptConfig 加载提示词（系统文件 + 自定义文件合并）。
func providePromptConfig(paths promptPaths) (*config.PromptConfig, error) {
	return config.LoadPromptConfig(paths.System, paths.Custom)
}

// provideOneBot 建 NapCat HTTP 客户端。
// onebot.NewClient 收两个 string，Wire 分不清哪个是地址哪个是 token，所以包一层。
func provideOneBot(loaded loadedConfig) *onebot.Client {
	return onebot.NewClient(loaded.NapCatHTTPAPI, loaded.NapCatAccessToken)
}

// provideBotNickname 取机器人昵称。
//
// 失败不致命（只告警、返回空昵称），与改造前一致：昵称只影响 @文本提及 的识别，
// QQ 号那条路径始终有效。**必须在快照构造之前**完成——昵称要作为不可变快照的一部分
// 发布出去，而不是发布之后再往快照里写。
func provideBotNickname(obClient *onebot.Client) botNickname {
	info, err := obClient.GetLoginInfo()
	if err != nil {
		logutil.Warn("获取机器人昵称失败，@检测仅使用QQ号", "err", err)
		return ""
	}
	logutil.Info("机器人昵称", "nickname", info.Nickname)
	return botNickname(info.Nickname)
}

// provideBuiltinImageMCP 在 llm.image_max>0 时启动内嵌「看图」MCP 服务（提供 view_image）。
//
// 它绑端口，是这里唯一有真实副作用的 provider；但它的产物确实要进依赖图——
// 地址会被 derive 写进快照，不经过图就只能靠全局变量传递。
// 启动失败不致命（与改造前一致）：只记日志、返回 nil，本会话没有 view_image 工具。
func provideBuiltinImageMCP(loaded loadedConfig) *mcpserver.Server {
	if loaded.LLMConfig.ImageMax <= 0 {
		return nil
	}

	builtinServer := mcpserver.New(nil, loaded.McpBuiltinToken)
	addr, err := builtinServer.Start()
	if err != nil {
		logutil.Error("内嵌 MCP 服务(看图)启动失败，本会话无 view_image 工具", "err", err)
		return nil
	}
	if !loaded.MCPConfig.Enabled {
		logutil.Warn("启用看图(llm.image_max>0)自动开启 mcp.enabled，注入内嵌 view_image 服务")
	}
	logutil.Info("内嵌 MCP 服务(看图)已启动", "url", addr, "带Bearer鉴权", loaded.McpBuiltinToken != "")
	return builtinServer
}

// provideBuiltinImageAddr 从服务句柄取出实际监听地址，供 derive 写进快照。
//
// 拆成两个 provider 是被 Wire 逼的：它只认"第二个返回值是 error 或 func()"的多返回值。
// 拆开其实更清楚——"起服务"和"问它监听在哪"本来就是两件事。
func provideBuiltinImageAddr(builtinServer *mcpserver.Server) builtinImageAddr {
	if builtinServer == nil {
		return ""
	}
	return builtinImageAddr(builtinServer.Addr())
}

// provideDerivedConfig 把"只有本进程知道"的运行期值贴到配置上，得到可以发布的快照内容。
func provideDerivedConfig(loaded loadedConfig, nickname botNickname, addr builtinImageAddr) *config.Config {
	return deriveConfig(loaded.Config, nickname, addr)
}

// provideConfigStore 建快照存储。之后所有消费者读到的都是这里发布出去的版本。
func provideConfigStore(initial *config.Config) *store.Store[config.Config] {
	return store.NewStore(initial)
}

// provideConfigSnapshot 把 Store.Get 的方法值当作 config.Snapshot 提供出去。
//
// 包一层而不是直接用配置存储：config.Snapshot 是 func() *Config，
// 消费者拿到的是"取当前快照"这个动作，而不是一个看起来恒定的指针。
func provideConfigSnapshot(target *store.Store[config.Config]) config.Snapshot {
	return target.Get
}

// provideWebServer 建 Web 管理面板；web_port<=0 时返回 nil（面板整体禁用）。
//
// 条件构造只能放进 provider：Wire 没有"按运行期条件二选一"的表达能力。
// 返回 nil 是它的惯用写法——下游要么判空，要么像 App 那样把 nil 当作"没启用"。
func provideWebServer(cfg config.Snapshot, obClient *onebot.Client) *webserver.Server {
	if cfg().WebPort <= 0 {
		return nil
	}
	return webserver.New(cfg, obClient)
}

// provideMetricsServer 建指标端点；metrics_addr 配成 off 时返回 nil。
func provideMetricsServer(addr string, registry *telemetry.Registry) *telemetry.MetricsServer {
	if addr == config.MetricsAddrOff {
		return nil
	}
	return telemetry.NewMetricsServer(addr, registry)
}

// provideConfigInformer 建配置监听器。
//
// list 里走的是 deriveConfig 而**不是**裸的 config.Load：昵称与内嵌看图地址
// 不在任何文件里，每次重读磁盘都必须重新贴上去，否则一次热重载之后
// @检测会失灵、view_image 会消失（都不报错，只是悄悄没了）。
func provideConfigInformer(sources config.Sources, target *store.Store[config.Config],
	nickname botNickname, addr builtinImageAddr) *store.Informer[config.Config] {
	informer := store.NewInformer(target, func() (*config.Config, error) {
		reloaded, err := config.Load(sources)
		if err != nil {
			return nil, err
		}
		return deriveConfig(reloaded, nickname, addr), nil
	}, store.NewFilePoller(configWatchInterval, sources.Config, sources.Secret))
	// AddEventHandler 是构造之后的一次方法调用，Wire 表达不了，所以在这个 provider 里做掉
	informer.AddEventHandler(warnRestartOnlyChanges)
	return informer
}

// providePromptObserver 建提示词监听器。
// 它只观察"文件变了"，不另存快照——提示词数据由 PromptConfig 自己持有并原子发布。
func providePromptObserver(paths promptPaths, promptCfg *config.PromptConfig,
	messageRouter *router.Router) *store.Observer {
	return store.NewObserver(
		store.NewFilePoller(configWatchInterval, paths.System, paths.Custom),
		func() { reloadPrompts(promptCfg, messageRouter) },
	)
}

// provideApp 组装 App。
//
// 这里用自定义 provider 而**不是** wire.Struct：
//
//   - wire.Struct 只能填导出字段，而 configInformer / promptObserver 是非导出的，
//     它们又必须被 Start() 用到；
//   - App 上有两类东西混在一起——"依赖图里的节点"与"生命周期状态"
//     （ctx / stop / closeOnce）。后者由 build.go 在拿到 ctx 之后赋值，
//     不属于图的一部分，wire.Struct 也没法区分。
//
// 于是：图里的东西全部经参数进来、在这里落位；生命周期状态留给 build.go 收尾。
func provideApp(
	cfg config.Snapshot,
	promptCfg *config.PromptConfig,
	llmClient llm.Client,
	obClient *onebot.Client,
	mcpManager *mcpclient.Manager,
	builtinMCP *mcpserver.Server,
	messageRouter *router.Router,
	messageBot *bot.Bot,
	web *webserver.Server,
	metrics *telemetry.MetricsServer,
	configInformer *store.Informer[config.Config],
	promptObserver *store.Observer,
) *App {
	return &App{
		Config:         cfg,
		Prompt:         promptCfg,
		LLM:            llmClient,
		OneBot:         obClient,
		MCP:            mcpManager,
		BuiltinMCP:     builtinMCP,
		Router:         messageRouter,
		Bot:            messageBot,
		Web:            web,
		Metrics:        metrics,
		configInformer: configInformer,
		promptObserver: promptObserver,
		webURL:         fmt.Sprintf("http://localhost:%d", cfg().WebPort),
	}
}
