package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"good-review-master/config"
	"good-review-master/logutil"

	"github.com/gin-gonic/gin"
)

// Server Web 管理面板服务器
type Server struct {
	cfg        *config.Config
	httpServer *http.Server
}

// New 创建 Web 服务器实例
func New(cfg *config.Config) *Server {
	gin.SetMode(gin.ReleaseMode)
	engine := gin.New()

	// 加载嵌入模板
	engine.LoadHTMLFS(http.FS(templateFS), "templates/*")

	// 提供静态资源
	engine.StaticFS("/static", http.FS(staticFS))

	// 全局中间件
	engine.Use(RecoveryMiddleware())
	engine.Use(LoggerMiddleware())

	s := &Server{cfg: cfg}

	// 注册路由
	engine.GET("/", handleIndex)
	engine.GET("/groups", makeHandleGroups(cfg))
	engine.GET("/groups/:id", makeHandleMessages(cfg))

	apiGroup := engine.Group("/api")
	{
		apiGroup.GET("/groups", makeHandleAPIGroups())
		apiGroup.GET("/groups/:id", makeHandleAPIMessages())
	}

	addr := fmt.Sprintf(":%d", cfg.WebPort)
	s.httpServer = &http.Server{
		Addr:         addr,
		Handler:      engine,
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	logutil.Info("Web 管理面板已就绪", "addr", fmt.Sprintf("http://localhost%s", addr))
	return s
}

// Start 启动 Web 服务（阻塞执行）
func (s *Server) Start() error {
	if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown 优雅关闭 Web 服务
func (s *Server) Shutdown(ctx context.Context) error {
	logutil.Info("正在关闭 Web 管理面板...")
	return s.httpServer.Shutdown(ctx)
}
