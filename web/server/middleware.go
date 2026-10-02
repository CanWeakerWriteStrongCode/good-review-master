package server

import (
	"net/http"
	"strings"
	"time"

	"good-review-master/logutil"

	"github.com/gin-gonic/gin"
)

// LoggerMiddleware 请求日志中间件：记录 method、path、status code、耗时
func LoggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		startTime := time.Now()
		c.Next()
		duration := time.Since(startTime)
		logutil.Info("HTTP 请求",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", duration.Milliseconds(),
		)
	}
}

// RecoveryMiddleware panic 恢复中间件（增强日志格式与项目 logutil 统一）
func RecoveryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if err := recover(); err != nil {
				logutil.Error("Web 请求 panic", "path", c.Request.URL.Path, "err", err)
				c.AbortWithStatus(500)
			}
		}()
		c.Next()
	}
}

// CORSMiddleware 跨域中间件：只放行 allowedOrigins 里列出的来源。
//
// 默认（allowedOrigins 为空）完全不下发 CORS 头 = 仅同源可访问，这是面板的常态：
// 前端是 embed 进二进制、与 API 同源的 H5，小程序不受 CORS 约束，
// 开发时 Vite dev server 走 proxy（见 web/frontend/vite.config.ts）也不产生跨域。
// 只有把面板嵌到别的站点时才需要在 runtime.cors_origins 里显式列出来源。
//
// 传入 "*" 表示放行任意来源——回到改造前的行为，仅在明确需要时使用。
func CORSMiddleware(allowedOrigins []string) gin.HandlerFunc {
	allowed := make(map[string]struct{}, len(allowedOrigins))
	allowAny := false
	for _, origin := range allowedOrigins {
		if origin == "*" {
			allowAny = true
			continue
		}
		allowed[origin] = struct{}{}
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin != "" {
			if _, ok := allowed[origin]; ok {
				// 反射具体来源（而非回 "*"）时，必须带 Vary: Origin，
				// 否则中间缓存可能把 A 站的响应喂给 B 站
				c.Header("Access-Control-Allow-Origin", origin)
				c.Header("Vary", "Origin")
			} else if allowAny {
				c.Header("Access-Control-Allow-Origin", "*")
			}
			if c.Writer.Header().Get("Access-Control-Allow-Origin") != "" {
				c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")
			}
		}

		// 预检直接结束，不进业务路由
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// AuthMiddleware JWT 校验中间件（无免鉴权通道）
func AuthMiddleware(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		if _, err := ParseJWT(token, secret); err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"code": 401, "data": nil})
			return
		}
		c.Next()
	}
}
