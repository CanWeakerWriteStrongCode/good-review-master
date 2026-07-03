package server

import (
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
