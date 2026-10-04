package server

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/onebot"

	"github.com/gin-gonic/gin"
)

// Server Web 管理面板服务器
type Server struct {
	cfg          *config.Config
	engine       *gin.Engine
	httpServer   *http.Server
	obClient     *onebot.Client
	groupNames   map[string]string // groupID → groupName 缓存
	groupNamesMu sync.RWMutex
	loginLimiter *loginRateLimiter
	draining     atomic.Bool // 优雅关闭开始后置位，/readyz 据此返回 503
	startedAt    time.Time   // 供 /api/diagnostics 报运行时长
}

// New 创建 Web 服务器实例
func New(cfg *config.Config, obClient *onebot.Client) *Server {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()

	// 不信任任何代理头：gin 默认会采信 X-Forwarded-For，那样 c.ClientIP() 就成了
	// 请求方完全可控的值，登录限流只要每次换个假 IP 就能绕过。
	// 面板默认直连（内嵌同源 H5），这样最安全。
	// 若将来确实放在反代后面，要改成 SetTrustedProxies([]string{"反代IP"}) 而不是放开全部。
	_ = engine.SetTrustedProxies(nil)

	// 全局中间件
	engine.Use(RecoveryMiddleware())
	engine.Use(LoggerMiddleware())
	engine.Use(CORSMiddleware(cfg.CorsOrigins))

	s := &Server{
		cfg:          cfg,
		engine:       engine,
		obClient:     obClient,
		groupNames:   make(map[string]string),
		loginLimiter: newLoginRateLimiter(),
		startedAt:    time.Now(),
	}

	// 探针：无鉴权，注册在 engine 根上（不走 /api 的鉴权中间件）
	engine.GET("/healthz", s.handleHealthz())
	engine.GET("/readyz", s.handleReadyz())

	// API 路由
	apiGroup := engine.Group("/api")
	apiGroup.POST("/login", handleLogin(cfg.WebUsername, cfg.WebPassword, cfg.JWTSecret, s.loginLimiter))
	apiGroup.Use(AuthMiddleware(cfg.JWTSecret))
	{
		apiGroup.GET("/status", handleAPIStatus(cfg))
		apiGroup.GET("/groups", handleAPIGroups(cfg, obClient, s.groupNames, &s.groupNamesMu))
		apiGroup.GET("/groups/:id", handleAPIMessages(s.groupNames, &s.groupNamesMu))
		apiGroup.POST("/logout", handleLogout())
		// 运行时诊断：恒定可用，不受 enable_pprof 影响。
		// goroutine 概览走 runtime.Stack 自取，没有采样式开销，没必要跟着 pprof 一起关。
		apiGroup.GET("/diagnostics", handleDiagnostics(cfg, s.startedAt))

		if cfg.EnablePprof {
			registerPprof(apiGroup.Group("/debug"))
			logutil.Warn("pprof 已开放", "path", "/api/debug/pprof/", "提示", "需 JWT；用完建议关掉 runtime.enable_pprof")
		}
	}

	// SPA fallback：非 /api/* 路径返回前端静态文件
	engine.NoRoute(s.serveFrontend)

	addr := fmt.Sprintf(":%d", cfg.WebPort)
	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      engine,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	logutil.Info("Web API 服务已就绪", "addr", fmt.Sprintf("http://localhost%s", addr))
	return s
}

// frontendFilePath 将请求路径映射到嵌入文件系统中的实际路径
func frontendFilePath(requestPath string) string {
	p := strings.TrimPrefix(requestPath, "/")
	if p == "" {
		p = "index.html"
	}
	return "static/frontend/" + p
}

// serveFrontend 提供前端 SPA 静态文件服务
func (s *Server) serveFrontend(c *gin.Context) {
	filePath := frontendFilePath(c.Request.URL.Path)

	data, err := frontendFS.ReadFile(filePath)
	if err != nil {
		// 文件不存在，fallback 到 index.html（SPA 路由）
		data, err = frontendFS.ReadFile("static/frontend/index.html")
		if err != nil {
			c.String(http.StatusOK, "前端资源未构建。开发模式请启动 Vite dev server，访问 http://localhost:8080/api/status 查看 API。")
			return
		}
		c.Data(http.StatusOK, "text/html; charset=utf-8", data)
		return
	}

	// 根据实际文件扩展名检测 MIME 类型
	contentType := mime.TypeByExtension(filepath.Ext(filePath))
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	c.Data(http.StatusOK, contentType, data)
}

// Start 启动 Web 服务（阻塞执行）
func (s *Server) Start() error {
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown 优雅关闭 Web 服务，顺序对优雅下线是本质的：
//
//  1. 置 draining → /readyz 立刻转 503
//  2. 等 shutdown_delay_sec（默认 0）
//  3. 才停监听、等在途请求收尾
//
// 第 2 步不能省：http.Server.Shutdown 会**立即关闭监听器**，之后再来的探针
// 连连接都建不起来，503 就没有任何观察窗口——等于白标。只有先亮出"我不ready了"、
// 留出时间让调用方（k8s endpoints、LB、nginx upstream）把流量摘走，再停监听，
// 顺序才成立。
//
// 所以这个等待**只在有调用方观察时才有意义**（k8s/LB 后面）。
// 直连单机跑的时候没有观察者，等待纯属拖慢退出，因此默认 0 = 不等待、行为与改造前一致。
func (s *Server) Shutdown(ctx context.Context) error {
	s.draining.Store(true)

	if delay := s.cfg.ShutdownDelay; delay > 0 {
		logutil.Info("已置为 not-ready，等待流量摘除后再停监听", "等待", delay.String())
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			// 上层给的关闭预算用完了，跳过等待直接关，不要卡住整个退出流程
			logutil.Warn("等待流量摘除被取消，直接关闭监听", "err", ctx.Err())
		}
	}

	logutil.Info("正在关闭 Web 管理面板...")
	return s.httpServer.Shutdown(ctx)
}

// HasFrontend 检查前端构建产物是否已嵌入
func (s *Server) HasFrontend() bool {
	_, err := frontendFS.ReadFile("static/frontend/index.html")
	return err == nil
}
