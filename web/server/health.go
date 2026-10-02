package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// 探针端点，对标 K8s 的 liveness / readiness 约定。
//
// 两个端点都**不加鉴权**：探针是由编排系统（k8s kubelet、docker healthcheck、
// 负载均衡）发起的，它们拿不到业务 token。因此响应体只允许固定字符串，
// 绝不能回显配置值、群号、密钥、上游地址——这是无鉴权端点的硬约束。
//
// 两者分工（这也是 K8s 的核心区分）：
//   - /healthz = liveness：进程还活着吗？答错会被重启，所以它只该反映"进程死没死"，
//     绝不能把外部依赖算进去，否则 NapCat 一抖动整个进程就被重启。
//   - /readyz = readiness：现在能接新活吗？答错只会被摘出流量，所以可以收紧。
//     进程开始优雅关闭时立刻转 503，让调用方别再往这里发新请求。
//
// 特意**没有**把"NapCat 是否可达""MCP 会话是否在线"放进 /readyz：
//   - 它们都是外部依赖，失败意味着能力降级（没有工具、暂时收不到消息），
//     而不是"不该被调用"；把降级当不可用会让机器人被无谓地反复摘流量。
//   - 这里是无鉴权端点，每次探测都外呼一次 NapCat（客户端超时 10s、重试 2 次）
//     等于给了一个免鉴权的放大接口。
// 真要观测这些，用 /api/status（已鉴权）或阶段 5 的 Prometheus 指标。

// handleHealthz 存活探针：能返回就说明进程在跑。
func (s *Server) handleHealthz() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

// handleReadyz 就绪探针：正在优雅关闭时返回 503，其余情况 200。
func (s *Server) handleReadyz() gin.HandlerFunc {
	return func(c *gin.Context) {
		if s.draining.Load() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "draining"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}
