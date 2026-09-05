package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"good-review-master/apppath"
	"good-review-master/bot"
	"good-review-master/cmd"
	"good-review-master/config"
	"good-review-master/internal/testutil"
	"good-review-master/llm"
	"good-review-master/logutil"
	"good-review-master/mcpclient"
	"good-review-master/onebot"
	"good-review-master/version"
	webserver "good-review-master/web/server"
)

func main() {
	logutil.SetupLogger()
	logutil.Info("版本：" + version.String())

	// 1. 检测并补全缺失的模板配置文件
	if config.InitDefaultFiles() {
		fmt.Println("已创建 config.yaml 文件，请配置 config.yaml 后再次启动程序，按回车键退出...")
		fmt.Scanln()
		os.Exit(0)
	}

	// 2. 加载主配置
	cfg, err := config.LoadConfig(apppath.ResolvePath("config.yaml"))
	if err != nil {
		logutil.Error("加载配置失败", "err", err)
		os.Exit(1)
	}

	// 3. 加载提示词配置
	systemPromptPath := apppath.ResolvePath("prompt_system.yaml")
	customPromptPath := config.CustomPromptPath(systemPromptPath)
	promptCfg, err := config.LoadPromptConfig(systemPromptPath, customPromptPath)
	if err != nil {
		logutil.Error("加载提示词配置失败", "err", err)
		os.Exit(1)
	}

	// 4. 创建大模型客户端（测试模式用 FakeLLM，不走真实 API）
	testMode := os.Getenv("GOOD_REVIEW_TEST") == "1"
	var llmClient llm.Client
	var fakeLLM *testutil.FakeLLM
	if testMode {
		fakeLLM = testutil.NewFakeLLM()
		llmClient = fakeLLM
		logutil.Warn("测试模式已启用：使用 FakeLLM，NapCat 指向死地址")
	} else {
		switch cfg.LLMConfig.Provider {
		case "openai":
			llmClient = llm.NewOpenAIAdapter(
				cfg.LLMConfig.APIKey,
				cfg.LLMConfig.APIBase,
				cfg.LLMConfig.ModelName,
				cfg.LLMConfig.Temperature,
				cfg.LLMConfig.TopP,
			)
		default:
			logutil.Error("不支持的大模型提供商", "provider", cfg.LLMConfig.Provider)
			os.Exit(1)
		}
	}

	// 5. 创建 OneBot HTTP 客户端
	obClient := onebot.NewClient(cfg.NapCatHTTPAPI, cfg.NapCatAccessToken)

	// 6. shutdown context（MCP 会话与路由 goroutine 都挂在它下面，退出时一并收摊）
	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 7. 启动 MCP 工具服务：后台并发连接每个服务并自动拉取工具清单（tools/list），
	// 拉到后原子更新快照，之后每次对话自动把 inject 服务的工具作为 function calling 注入。
	// 不阻塞启动：连不上的服务由重连循环按 retry_interval_sec 接管。
	mcpMgr := mcpclient.New(cfg.MCPConfig, shutdownCtx)
	// 初始化：先把配置里所有 MCP 服务以 Info 打出来，再后台建连
	mcpMgr.LogConfig()
	mcpMgr.Start()

	// 8. 创建指令路由器（传入 shutdown context，goroutine 通过 errgroup 自动继承）
	router := cmd.NewRouter(cfg, promptCfg, llmClient, obClient, mcpMgr, shutdownCtx)

	// 9. 获取机器人昵称
	if info, err := obClient.GetLoginInfo(); err != nil {
		logutil.Warn("获取机器人昵称失败，@检测仅使用QQ号", "err", err)
	} else {
		cfg.BotNickname = info.Nickname
		logutil.Info("机器人昵称", "nickname", cfg.BotNickname)
	}

	logutil.Info("🚀 【不是好评大师】机器人启动成功")
	logutil.Info("机器人QQ：" + cfg.BotQQ)
	logutil.Info("允许响应群：" + cfg.AllowGroupsStr())
	logutil.Info("NapCat HTTP API：" + cfg.NapCatHTTPAPI)

	// 10. 创建机器人并启动轮询（支持优雅退出）
	botInstance := bot.NewBot(cfg, obClient, router)
	go botInstance.RunPollingLoop(shutdownCtx)

	// 11. 启动 Web 管理面板（web_port > 0 时启用；账号密码必填，无免密模式）
	var webSrv *webserver.Server
	if cfg.WebPort > 0 {
		if cfg.WebUsername == "" || cfg.WebPassword == "" {
			logutil.Error("Web 管理面板已启用（web_port>0）但未设置 web_username/web_password，请在 config.yaml 中配置登录账号和密码")
			os.Exit(1)
		}
		webSrv = webserver.New(cfg, obClient)
		if testMode {
			webSrv.EnableDebug(router, fakeLLM)
		}
		go func() {
			if err := webSrv.Start(); err != nil {
				logutil.Error("Web 服务异常退出", "err", err)
			}
		}()
		logutil.Info("Web 管理面板已启动", "addr", fmt.Sprintf("http://localhost:%d", cfg.WebPort))
	}

	<-shutdownCtx.Done()
	logutil.Info("收到退出信号，正在关闭...")

	// 12. 关闭 Web 管理面板
	if webSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := webSrv.Shutdown(ctx); err != nil {
			logutil.Error("Web 服务关闭失败", "err", err)
		}
	}

	if err := router.Wait(); err != nil {
		logutil.Error("等待 goroutine 退出失败", "err", err)
	}

	// 13. 关闭 MCP 会话（stdio 子进程随之终止）
	if err := mcpMgr.Close(); err != nil {
		logutil.Error("关闭 MCP 服务失败", "err", err)
	}
	logutil.Info("已安全退出")
}
