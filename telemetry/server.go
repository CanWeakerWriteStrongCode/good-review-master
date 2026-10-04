package telemetry

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// NewRegistry 建立只含本程序指标的注册表。
//
// 刻意**不用** Prometheus 的默认全局注册表：默认注册表是个进程级单例，
// 任何第三方库都可以往里塞指标，测试也无法隔离（同一个 collector 注册两次会 panic）。
// 自己建一个，注册什么就导出什么，一目了然。
//
// 每次调用都返回全新注册表，因此反复调用是安全的（测试里可以随意建）。
func NewRegistry() *Registry {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),                                       // go_*：goroutine 数、堆、GC
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}), // process_*：CPU、RSS、fd
	)
	registry.MustRegister(allCollectors()...)
	return &Registry{registry: registry}
}

// Registry 包一层 prometheus.Registry，只是为了让 app 不必 import prometheus。
type Registry struct {
	registry *prometheus.Registry
}

// Gatherer 暴露底层注册表（给 promhttp 用）。
func (r *Registry) Gatherer() prometheus.Gatherer { return r.registry }

// MetricsServer 指标端点，独立于业务 Web 面板另起一个监听器。
//
// 为什么独立监听而不是挂在业务端口上：
//   - 面板要过 JWT，而 Prometheus 抓取带不了（也不会去带）JWT；
//   - 指标是运维面的东西，不该跟着面板端口一起开合——面板关掉时指标仍要能用
//     （web_port<=0 时本进程依然是一个在跑的机器人，依然值得被观测）。
//
// 默认只绑 127.0.0.1（见 runtime.metrics_addr），不对外暴露。
type MetricsServer struct {
	addr       string
	httpServer *http.Server
}

// NewMetricsServer 构造指标端点。registry 由 NewRegistry 提供。
func NewMetricsServer(addr string, registry *Registry) *MetricsServer {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(registry.Gatherer(), promhttp.HandlerOpts{}))

	return &MetricsServer{
		addr: addr,
		httpServer: &http.Server{
			Addr:    addr,
			Handler: mux,
			// 抓取是短请求，但 /metrics 可能较大；给足写超时、不设读超时压力
			ReadHeaderTimeout: 5 * time.Second,
			WriteTimeout:      30 * time.Second,
			IdleTimeout:       60 * time.Second,
		},
	}
}

// Addr 返回监听地址，供启动日志展示。
func (s *MetricsServer) Addr() string { return s.addr }

// Start 启动指标端点（阻塞执行，由调用方放进 goroutine）。
func (s *MetricsServer) Start() error {
	if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown 优雅关闭指标端点。
// 没有"先置不就绪"那一步：Prometheus 抓不到就抓不到，它对一次失败的抓取
// 只会留下一个缺口，不像流量入口那样必须把请求排空。
func (s *MetricsServer) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}
