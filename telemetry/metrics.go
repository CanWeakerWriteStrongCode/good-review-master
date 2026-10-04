// Package telemetry 是本程序唯一的指标（Prometheus）与链路（OpenTelemetry）出口。
//
// 两种数据同住一个包，是因为它们的生命周期完全一致：同一份配置开关、同一次初始化、
// 同一次 Shutdown。拆成 metric/ 与 trace/ 只会让 app 多出一组必须手动保持同步的调用。
//
// **本包是叶子包**：只依赖 prometheus / otel 与标准库，不 import 任何本项目内部包。
// 反过来，底层包（pool / cache / llm / mcpclient / ...）可以直接 import 它来埋点。
// 这是有意的取舍：指标天然是进程级单例，为它套一层接口再注入进每个包，
// 换来的那点"解耦"并不比"直接调用一个包级变量"更清晰，却要多改十几个构造函数。
//
// 指标在包级变量里**声明但不注册**：注册由 NewRegistry 显式完成（本仓库不允许 init 副作用）。
// 未注册的 collector 依然可写、可观察，只是不会被导出——所以埋点在任何初始化顺序下都不会 panic，
// 测试里也不必先起 telemetry 才能调用。
package telemetry

import "github.com/prometheus/client_golang/prometheus"

// result 标签的两个取值。集中定义是为了避免各处手拼字符串写错
// （写错不会报错，只会多出一条永远为 0 的时间线）。
const (
	ResultOK    = "ok"
	ResultError = "error"
)

// UnmatchedRoute 是 route 标签在"没命中任何路由"时的固定取值（见 HTTPRouteLabel）。
const UnmatchedRoute = "unmatched"

// reason 标签的取值：本地保护把一次大模型调用挡在了门外。
const (
	ReasonRateLimited = "rate_limited"
	ReasonCircuitOpen = "circuit_open"
)

var (
	// ---------------- HTTP ----------------

	// HTTPRequestsTotal HTTP 请求数，按 方法 / 路由模板 / 状态码 分组。
	HTTPRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "HTTP 请求数，按 method/route/status 分组",
	}, []string{"method", "route", "status"})

	// HTTPRequestDurationSeconds HTTP 请求耗时。
	HTTPRequestDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "HTTP 请求耗时（秒），按路由模板分组",
		Buckets: []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	}, []string{"route"})

	// ---------------- 大模型 ----------------

	// LLMRequestsTotal 大模型调用次数。model 用配置里的模型名，
	// 换模型时曲线会另起一条，便于对比"同一段时间两个模型各自表现如何"。
	LLMRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_requests_total",
		Help: "大模型调用次数，按 model/result 分组",
	}, []string{"model", "result"})

	// LLMDurationSeconds 大模型调用耗时。
	LLMDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "llm_duration_seconds",
		Help:    "大模型调用耗时（秒），按 model 分组",
		Buckets: []float64{0.25, 0.5, 1, 2, 5, 10, 20, 30, 60, 120},
	}, []string{"model"})

	// LLMTokensTotal 大模型 token 用量，kind = prompt | completion。
	// 它拿的是服务端返回的 usage，不是本地估算——本地估算只用于选窗决策。
	LLMTokensTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_tokens_total",
		Help: "大模型 token 用量，按 kind(prompt/completion) 分组",
	}, []string{"kind"})

	// LLMRejectedTotal 被本地保护拦下、根本没发出去的大模型调用次数。
	//
	// 它与 LLMRequestsTotal 的关系是"没进门的"与"进了门的"：
	// 只看失败率（result=error）会把熔断期间的表现算成"没有请求"，
	// 于是故障看起来像是流量消失了。这条计数器就是那部分消失的流量。
	LLMRejectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_rejected_total",
		Help: "被本地限速/熔断拦下、未发出的大模型调用次数，按 reason 分组",
	}, []string{"reason"})

	// LLMCostTotal 大模型累计成本（相对单位）。
	// 本项目配置里的单价是相对值（见 docs/cache-cost-analysis.md），
	// 所以这里累加的也是"相对成本"，用途是看趋势与对比两种窗口模式，不是算钱。
	LLMCostTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "llm_cost_total",
		Help: "大模型累计成本（相对单位，由缓存命中/未命中单价折算）",
	})

	// ---------------- 缓存窗口（本项目的核心业务指标） ----------------

	// LLMWindowModeTotal 每次锐评选了哪种窗口：extend（吃缓存命中）还是 reset。
	// 这条曲线是缓存成本模型是否按预期工作的唯一直接证据。
	LLMWindowModeTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "llm_window_mode_total",
		Help: "选窗决策结果，按 mode(extend/reset) 分组",
	}, []string{"mode"})

	// LLMCacheHitTotal 累计命中的前缀 token 数（只有 extend 路径会命中）。
	LLMCacheHitTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "llm_cache_hit_total",
		Help: "累计命中的缓存前缀 token 数",
	})

	// CacheMessages 各群当前缓存的消息条数。
	//
	// 这里的 group 标签基数**有界**：只会为白名单里的群产生时间线（allow_groups 是显式配置的短名单）。
	// 但它是全仓库唯一一个按业务 ID 打标签的指标，加新指标时不要照抄这个做法——
	// route / status 这类无界标签会随请求内容增长，是 metrics 落地的经典事故。
	CacheMessages = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "cache_messages",
		Help: "各群环形缓存当前消息条数",
	}, []string{"group"})

	// ---------------- 轮询 ----------------

	// PollCycleDurationSeconds 一轮轮询（拉取全部白名单群）的耗时。
	PollCycleDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "poll_cycle_duration_seconds",
		Help:    "一轮轮询全部群的耗时（秒）",
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2, 5, 10},
	})

	// PollLagSeconds 消息从发出到被我们处理的延迟（秒）。
	//
	// 定义要说清楚，否则会误读："最新一条消息的时间戳到本轮处理的时刻"。
	// 正常运行时它约等于一个轮询间隔；若 NapCat 卡住或我们在返回历史消息，
	// 它会持续抬升——那正是"我们落后群聊多久"的信号。
	PollLagSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "poll_lag_seconds",
		Help:    "新消息从发出到被处理的延迟（秒）",
		Buckets: []float64{0.5, 1, 2, 5, 10, 20, 60, 300},
	})

	// PollFetchErrorsTotal 拉取群消息失败的次数。
	PollFetchErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "poll_fetch_errors_total",
		Help: "拉取群消息失败次数，按 group 分组",
	}, []string{"group"})

	// ---------------- MCP ----------------

	// MCPToolCallsTotal MCP 工具调用次数。
	MCPToolCallsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "mcp_tool_calls_total",
		Help: "MCP 工具调用次数，按 server/tool/result 分组",
	}, []string{"server", "tool", "result"})

	// MCPSessionsUp 各 MCP 服务当前是否在线（1=在线）。
	MCPSessionsUp = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "mcp_sessions_up",
		Help: "MCP 服务会话是否在线，1=在线",
	}, []string{"server"})

	// ---------------- 协程池 ----------------

	// PoolTasksRunning 正在执行的 worker 数（有界，上限是池大小）。
	PoolTasksRunning = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pool_tasks_running",
		Help: "协程池中正在执行任务的 worker 数",
	})

	// PoolQueueLen 队列中排队等待的任务数。
	// 它持续贴着实测上限，说明池子已经跟不上提交速度——下一步就是 Submit 返回 false。
	PoolQueueLen = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "pool_queue_len",
		Help: "协程池队列中等待的任务数",
	})

	// PoolSubmitRejectedTotal 因队列满而被拒绝的任务数。
	// 本项目里一次拒绝 = 一次锐评没能发出，是"该扩容/该降频"的硬信号。
	PoolSubmitRejectedTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "pool_submit_rejected_total",
		Help: "因队列满被拒绝的提交次数",
	})

	// ---------------- 配置重载 ----------------

	// ConfigReloadTotal 配置重载次数，source = auto | manual | resync。
	ConfigReloadTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "config_reload_total",
		Help: "配置重载次数，按 source(auto/manual/resync) 分组",
	}, []string{"source"})

	// ConfigReloadErrorsTotal 配置重读失败次数。
	// 失败时会沿用旧快照继续跑，所以它涨了但服务没挂——必须靠这条指标才看得见。
	ConfigReloadErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "config_reload_errors_total",
		Help: "配置重读失败次数（失败时沿用旧快照）",
	})

	// ConfigLastReloadTimestamp 最后一次成功重载的时间（Unix 秒）。
	ConfigLastReloadTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "config_last_reload_timestamp",
		Help: "最后一次成功重载配置的时间戳（Unix 秒）",
	})
)

// allCollectors 汇总本包声明的全部 collector，供 NewRegistry 一次性注册。
//
// 单独列一张表而不是把 NewRegistry 写成四十行 MustRegister：
// 新增指标时**只会漏在一处**（这里），而漏掉的表现是"指标写了但 /metrics 里没有"，
// 有一条集中清单更容易一眼核对。
func allCollectors() []prometheus.Collector {
	return []prometheus.Collector{
		HTTPRequestsTotal,
		HTTPRequestDurationSeconds,

		LLMRequestsTotal,
		LLMDurationSeconds,
		LLMTokensTotal,
		LLMRejectedTotal,
		LLMCostTotal,

		LLMWindowModeTotal,
		LLMCacheHitTotal,
		CacheMessages,

		PollCycleDurationSeconds,
		PollLagSeconds,
		PollFetchErrorsTotal,

		MCPToolCallsTotal,
		MCPSessionsUp,

		PoolTasksRunning,
		PoolQueueLen,
		PoolSubmitRejectedTotal,

		ConfigReloadTotal,
		ConfigReloadErrorsTotal,
		ConfigLastReloadTimestamp,
	}
}

// HTTPRouteLabel 把 gin 的 FullPath()（路由模板）转成 route 标签值。
//
// **绝不能用原始 URL**：/api/groups/<群号> 会让标签基数随群数量无限增长，
// 每条不同的路径在 Prometheus 里都是一个独立时间线，几天就能把内存吃光。
// 这是 metrics 落地最常见的坑，所以把它封成一个函数、配一条注释，而不是靠在各处自觉。
//
// fullPath 为空表示请求没命中任何已注册路由（404 等）。这种情况同样不能落原始 URL，
// 统一归到 UnmatchedRoute。
func HTTPRouteLabel(fullPath string) string {
	if fullPath == "" {
		return UnmatchedRoute
	}
	return fullPath
}
