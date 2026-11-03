# 指标与追踪

## 端点

| 项 | 地址 | 开关 |
| --- | --- | --- |
| Prometheus 指标 | `http://127.0.0.1:9100/metrics` | `runtime.metrics_addr`，填 `off` 关闭 |
| 链路追踪 | OTLP/HTTP 上报 | `runtime.otlp_endpoint`，留空关闭 |
| 健康探针 | `http://127.0.0.1:17090/healthz`、`/readyz` | 跟着 `web_port` 走 |

指标端口只绑本机、独立于面板：面板要过 JWT，Prometheus 抓取带不了 token；面板关掉时指标依然值得看。

抓取配置和面板定义在仓库根：

- `deploy/prometheus-scrape.yml` —— 一段 scrape 配置，拷进 Prometheus 的 `scrape_configs`。
- `deploy/grafana-dashboard.json` —— 导入 Grafana 即可用。

## 指标怎么分组看

### 1. 出问题先看这三个

| 指标 | 说明 | 怎么读 |
| --- | --- | --- |
| `pipeline_stage_backlog{stage}` | 各阶段队列里没被取走的消息数 | 持续抬升 = 这个阶段跟不上 |
| `pipeline_stage_batch_duration_seconds{stage}` | 处理一批的耗时 | Stage 3 分位数抬升说明模型变慢，不是消息变多 |
| `im_events_total{result}` | 入站事件按去向计数 | `accepted` 与 `ignored` 差值大 = 桥接层推了不该推的；`queue_full` 涨 = 下游堵了 |

### 2. 流水线

| 指标 | 说明 |
| --- | --- |
| `pipeline_stage_ready{stage}` | 等着派活的会话数。比 backlog 更能说明排队：积压 100 条可能只来自 1 个群 |
| `pipeline_stage_active{stage}` | 正在处理的会话数，上限就是该阶段并发 |
| `pipeline_stage_workers{stage}` | 池里的协程数；与 `active` 的差就是睡着的协程 |

### 3. 大模型

| 指标 | 说明 |
| --- | --- |
| `llm_requests_total{model,result}` | 调用次数与结果 |
| `llm_duration_seconds{model}` | 耗时分布 |
| `llm_tokens_total{kind}` | prompt / completion token 量 |
| `llm_cost_total{workspace}` | 按配置的单价折算的累计成本 |
| `llm_cache_hit_total{workspace}` | 累计命中的前缀 token |
| `llm_window_mode_total{workspace,mode}` | 选窗决策：`extend` 还是 `reset` |
| `llm_rejected_total{reason}` | 被限速或熔断拦下、根本没发出去的调用 |
| `llm_response_format_total{mode}` | 结构化输出实际用的档位（`json_schema` / `json_object` / `none`） |

`reset` 占比突然升高，先看两处：`cache_ttl_sec` 是不是太小（群聊稀疏时锚点总过期），或者 `cache_hit_cost` 是不是接近 `cache_miss_cost`（扩展永远算不过重置）。

### 4. 面板与接口

| 指标 | 说明 |
| --- | --- |
| `http_requests_total{method,route,status}` | 请求数 |
| `http_request_duration_seconds{route}` | 按路由模板的耗时 |
| `http_business_errors_total{route,code}` | 业务失败（`code != 200`）。UI 没报错但这里在涨，说明前端吞了错误 |

`route` 是路由模板（`/api/workspaces/:wid/groups`）而不是实际路径，避免标签爆炸。

### 5. 其它

| 指标 | 说明 |
| --- | --- |
| `cache_messages{workspace,group}` | 各群环形缓存当前条数 |
| `proactive_judge_total{workspace,result}` | 主动插话裁判结论：`yes` / `no` / `error` |
| `workspace_resolve_total{result}` | 入站事件的工作空间归属结果 |
| `mcp_tool_calls_total{server,tool,result}` / `mcp_sessions_up{server}` | MCP 工具调用与服务在线状态 |
| `pool_tasks_running` / `pool_queue_len` / `pool_submit_rejected_total` | 协程池。`submit_rejected` 涨 = 一次锐评没能发出 |
| `config_reload_total{source}` / `config_reload_errors_total` | 配置热重载。失败时沿用旧快照继续跑，只有这个指标看得见 |
| `poll_cycle_duration_seconds` / `poll_lag_seconds` / `poll_fetch_errors_total` | 拉取群消息的耗时、滞后与失败 |

## 链路追踪

`otlp_endpoint` 留空时所有埋点都是 no-op，没有开销。要看时起一个后端再指过来：

```bash
docker run --rm -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one
# config.yaml: runtime.otlp_endpoint: "http://localhost:4318"
```

进程退出时会最后冲刷一次 span，前面那些组件的关闭动作本身也会产生 span，所以追踪的关闭排在最后。

## 自检：链路是不是真通了

```bash
# 指标端点起来了吗
curl -s http://127.0.0.1:9100/metrics | Select-String "pipeline_stage_backlog"

# 面板和探针
curl -s http://127.0.0.1:17090/healthz
curl -s -o NUL -w "%{http_code}" http://127.0.0.1:17090/readyz

# 发一条消息后，收包与入队有没有动
curl -s http://127.0.0.1:9100/metrics | Select-String "im_events_total"
```

三个都要有数：`im_events_total{result="accepted"}` 在涨说明事件进来了；`pipeline_stage_backlog` 有值说明在排队；`llm_requests_total` 在涨说明真的调了模型。缺哪一环就从那一环往回查。

## 限速与熔断怎么看

两者都会让调用**立刻失败**而不是排队：`llm_rejected_total{reason}` 分 `rate_limit` 和 `breaker`。群里看到的是固定话术。

- 限速打开后持续被拒 → 上游配额配错了，或者并发给太高。
- 熔断反复打开 → 先看 `llm_requests_total{result="error"}` 和日志里的上游错误，不要先调 `breaker_failures`。

## 代码位置

| 文件 | 看什么 |
| --- | --- |
| `telemetry/metrics.go` | 所有指标的定义与每个标签的含义 |
| `telemetry/server.go` | 指标端口的监听与关闭 |
| `telemetry/trace.go` / `otlp.go` | TracerProvider 初始化与 OTLP 上报 |
| `web/server/middleware.go` | HTTP 指标埋点 |
| `deploy/prometheus-scrape.yml` / `deploy/grafana-dashboard.json` | 抓取与面板 |
