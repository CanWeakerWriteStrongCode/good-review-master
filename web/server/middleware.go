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

// CORSMiddleware 跨域中间件（开发环境用，生产也可保留用于多端访问）
func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}
		c.Next()
	}
}

// AuthMiddleware JWT 校验中间件。若未配置密码则直接放行。
func AuthMiddleware(password, secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if password == "" {
			c.Next()
			return
		}
		token := c.GetHeader("Authorization")
		if len(token) > 7 && token[:7] == "Bearer " {
			token = token[7:]
		}
		if _, err := ParseJWT(token, secret); err != nil {
			c.AbortWithStatusJSON(401, gin.H{"code": 401, "data": nil})
			return
		}
		c.Next()
	}
}
