package server

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"good-review-master/logutil"
	"good-review-master/telemetry"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// TelemetryMiddleware 给每个 HTTP 请求开一个 span 并记录指标。
//
// 两件事放在同一个中间件里，是因为它们都必须读**同样的三个值**（路由模板、状态码、耗时），
// 而这些值只有在 c.Next() 之后才齐。分成两个中间件就要把"开始时间"存进 gin.Context 传来传去，
// 反而更绕。
func TelemetryMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, span := telemetry.StartSpan(c.Request.Context(), "http.request",
			trace.WithSpanKind(trace.SpanKindServer))
		defer span.End()
		// 把带 span 的 ctx 挂回请求：处理器里取 c.Request.Context() 就能拿到，
		// 于是 HTTP 入口与它下游（如 debug trigger 触发的路由）是同一条链路。
		c.Request = c.Request.WithContext(ctx)

		startTime := time.Now()
		c.Next()
		duration := time.Since(startTime)

		// 路由模板只有匹配完成后才拿得到，所以这一行必须在 c.Next() 之后。
		route := telemetry.HTTPRouteLabel(c.FullPath())
		status := c.Writer.Status()

		telemetry.HTTPRequestsTotal.WithLabelValues(c.Request.Method, route, strconv.Itoa(status)).Inc()
		telemetry.HTTPRequestDurationSeconds.WithLabelValues(route).Observe(duration.Seconds())

		span.SetName(c.Request.Method + " " + route)
		span.SetAttributes(
			attribute.String("http.request.method", c.Request.Method),
			attribute.String("http.route", route),
			attribute.Int("http.response.status_code", status),
		)

		// 业务失败要在这一层认：它们的 HTTP 状态是 200，结论在 body.code 里，
		// 不加这一段的话，业务错误在上面那条 http_requests_total 上跟成功请求完全一样。
		if biz, ok := bizErrorOf(c); ok {
			telemetry.HTTPBusinessErrorsTotal.
				WithLabelValues(route, strconv.Itoa(int(biz.Code))).Inc()
			span.SetAttributes(
				attribute.Int("app.error.code", int(biz.Code)),
				attribute.String("app.error.id", biz.Code.messageID()),
			)
			if biz.Code.isServerFault() {
				// 只有服务器自己出问题才标红：密码输错、工作空间不存在都是正常现象，
				// 标红会把 trace 的错误率刷满，真正要看的那几条反而被淹掉。
				span.SetStatus(codes.Error, biz.Code.messageID())
			}
		} else if status >= http.StatusInternalServerError {
			// 只剩真·传输/服务故障走这条路（panic、进程内部错误）。
			// 4xx 仍然不标：探测、过期 token 会把错误率刷满。
			span.SetStatus(codes.Error, http.StatusText(status))
		}
	}
}

// LoggerMiddleware 请求日志中间件：记录 method、path、status code、耗时
func LoggerMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		startTime := time.Now()
		c.Next()
		duration := time.Since(startTime)
		logutil.InfoCtx(c.Request.Context(), "HTTP 请求",
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"duration_ms", duration.Milliseconds(),
		)
	}
}

// RecoveryMiddleware panic 恢复中间件。
//
// 它必须是**最外层**：panic 会打断 c.Next() 的返回链，其他中间件的后置代码
// （包括统一错误出口）根本不会执行，所以这里得自己把响应写出来。
func RecoveryMiddleware(locales *Locales) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logutil.Error("Web 请求 panic", "path", c.Request.URL.Path, "err", recovered)
				if c.Writer.Written() {
					// 响应已经开始写了，补不回来：直接断掉，让前端看到请求失败，
					// 总比返回一段被截断的 JSON 强
					c.Abort()
					return
				}
				// 走与普通未知错误同一个出口：响应里只有兜底句 + 单号，
				// 栈和消息体已经进了上面那条日志
				writeBizError(c, locales, fmt.Errorf("panic: %v", recovered))
				c.Abort()
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

// AuthMiddleware JWT 校验中间件（无免鉴权通道）。
//
// 失败时只登记、不自己拼响应：文案与语言由统一错误出口决定，
// 这里写死一句话就等于把中文又搬回了代码里。
func AuthMiddleware(secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		token := strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer ")
		if _, err := ParseJWT(token, secret); err != nil {
			fail(c, CodeUnauthorized, nil)
			return
		}
		c.Next()
	}
}
