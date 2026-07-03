package server

import (
	"context"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
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

	// 全局中间件
	engine.Use(RecoveryMiddleware())
	engine.Use(LoggerMiddleware())
	engine.Use(CORSMiddleware())

	s := &Server{cfg: cfg}

	// API 路由
	apiGroup := engine.Group("/api")
	{
		apiGroup.GET("/status", handleAPIStatus(cfg))
		apiGroup.GET("/groups", handleAPIGroups(cfg))
		apiGroup.GET("/groups/:id", handleAPIMessages(cfg))
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

// Shutdown 优雅关闭 Web 服务
func (s *Server) Shutdown(ctx context.Context) error {
	logutil.Info("正在关闭 Web 管理面板...")
	return s.httpServer.Shutdown(ctx)
}

// HasFrontend 检查前端构建产物是否已嵌入
func (s *Server) HasFrontend() bool {
	_, err := frontendFS.ReadFile("static/frontend/index.html")
	return err == nil
}
