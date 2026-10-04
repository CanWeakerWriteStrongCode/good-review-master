package app

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"good-review-master/bot"
	"good-review-master/config"
	"good-review-master/internal/testutil"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/mcpclient"
	"good-review-master/mcpserver"
	"good-review-master/onebot"
	"good-review-master/router"
	webserver "good-review-master/web/server"
)

// Options 装配输入（都是外部环境决定、组件自己拿不到的东西）。
type Options struct {
	ConfigPath       string // config.yaml 路径
	SecretPath       string // secret.yaml 路径（可以不存在）
	SystemPromptPath string // prompt_system.yaml 路径
	TestMode         bool   // true：用 FakeLLM 顶替真实大模型，并开放 /api/debug/* 自测接口
}

// New 装配整个应用：加载配置、构造各组件并接线，返回可直接 Run 的 App。
//
// 所有可能失败的步骤（配置、校验、大模型提供商）都排在占用任何资源之前，
// 因此 New 失败时调用方无需清理。
// 注意 New 并非无副作用的纯构造：内嵌看图 MCP 要在此绑定端口（地址得参与装配），
// MCP 建连也在这里发起，这样启动日志的顺序与改造前逐条一致。
func New(opts Options) (*App, error) {
	// 配置的合法性（含"开了 web_port 就必须有账号密码"）由 config.Load 内部的
	// 域内校验 + Config.Validate 一次性判定，New 不再自己重复检查一遍。
	cfg, err := config.Load(config.Sources{Config: opts.ConfigPath, Secret: opts.SecretPath})
	if err != nil {
		return nil, fmt.Errorf("加载配置失败：%w", err)
	}

	promptCfg, err := config.LoadPromptConfig(opts.SystemPromptPath, config.CustomPromptPath(opts.SystemPromptPath))
	if err != nil {
		return nil, fmt.Errorf("加载提示词配置失败：%w", err)
	}

	application := &App{Config: cfg, Prompt: promptCfg}

	// 大模型客户端（测试模式用 FakeLLM，不走真实 API）
	if opts.TestMode {
		application.FakeLLM = testutil.NewFakeLLM()
		application.LLM = application.FakeLLM
		logutil.Warn("测试模式已启用：使用 FakeLLM，NapCat 指向死地址")
	} else {
		switch cfg.LLMConfig.Provider {
		case "openai":
			application.LLM = llm.NewOpenAIAdapter(
				cfg.LLMConfig.APIKey,
				cfg.LLMConfig.APIBase,
				cfg.LLMConfig.ModelName,
				cfg.LLMConfig.Temperature,
				cfg.LLMConfig.TopP,
			)
		default:
			return nil, fmt.Errorf("不支持的大模型提供商：%s", cfg.LLMConfig.Provider)
		}
	}

	// shutdown context：MCP 会话、路由 goroutine、消息轮询都挂在它下面，退出时一并收摊
	application.ctx, application.stop = signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	application.OneBot = onebot.NewClient(cfg.NapCatHTTPAPI, cfg.NapCatAccessToken)

	application.setupBuiltinImageMCP()

	// MCP 工具服务：后台并发连接每个服务并自动拉取工具清单（tools/list），
	// 拉到后原子更新快照，之后每次对话自动把 inject 服务的工具作为 function calling 注入。
	// 不阻塞启动：连不上的服务由重连循环按 retry_interval_sec 接管。
	application.MCP = mcpclient.New(cfg.MCPConfig, application.ctx)
	application.MCP.LogConfig()
	application.MCP.Start()

	application.Router = router.NewRouter(cfg, promptCfg, application.LLM, application.OneBot, application.MCP, application.ctx)

	if info, err := application.OneBot.GetLoginInfo(); err != nil {
		logutil.Warn("获取机器人昵称失败，@检测仅使用QQ号", "err", err)
	} else {
		cfg.BotNickname = info.Nickname
		logutil.Info("机器人昵称", "nickname", cfg.BotNickname)
	}

	logutil.Info("🚀 【不是好评大师】机器人启动成功")
	logutil.Info("机器人QQ：" + cfg.BotQQ)
	logutil.Info("允许响应群：" + cfg.AllowGroupsStr())
	logutil.Info("NapCat HTTP API：" + cfg.NapCatHTTPAPI)

	application.Bot = bot.NewBot(cfg, application.OneBot, application.Router)

	if cfg.WebPort > 0 {
		application.Web = webserver.New(cfg, application.OneBot)
		if opts.TestMode {
			application.Web.EnableDebug(application.Router, application.FakeLLM)
		}
	}

	return application, nil
}

// setupBuiltinImageMCP 在 llm.image_max>0 时启动内嵌「看图」MCP 服务（提供 view_image），
// 并把它的地址注入 MCP 配置。
//
// 只监听 127.0.0.1；mcp_builtin_token 非空则服务端校验 Bearer，客户端带同一 token。
// 启动失败不致命：只记日志，本会话没有 view_image 工具。
// 注入直接写到 cfg.MCPConfig（而非拷贝），保证 Router 与 MCP Manager 看到同一份配置。
func (a *App) setupBuiltinImageMCP() {
	cfg := a.Config
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
		cfg.MCPConfig.Enabled = true
		logutil.Warn("启用看图(lm image_max>0)自动开启 mcp.enabled，注入内嵌 view_image 服务")
	}
	cfg.MCPConfig.Servers = append(append([]config.MCPServerConf{}, cfg.MCPConfig.Servers...),
		config.MCPServerConf{Name: "builtin_image", Transport: "http", URL: addr, Token: cfg.McpBuiltinToken})

	a.BuiltinMCP = builtinServer
	logutil.Info("内嵌 MCP 服务(看图)已启动", "url", addr, "带Bearer鉴权", cfg.McpBuiltinToken != "")
}
