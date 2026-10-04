# 监控与追踪

本程序把指标（Prometheus）与链路（OpenTelemetry）集中在 `telemetry/` 包里，
默认都是"能开就开着"的形态，且**不依赖任何外部组件**就能读：

- **指标默认开**（`runtime.metrics_addr` 默认 `127.0.0.1:9100`），`curl` 就能看；
- **链路默认关**（`runtime.otlp_endpoint` 默认空），关着时所有 span 都是 no-op，成本接近于零。

## 指标

```bash
curl -s http://127.0.0.1:9100/metrics | grep '^good\|^llm\|^http\|^poll\|^mcp\|^pool\|^config\|^cache'
```

（指标名不带 namespace 前缀，所以直接按各域前缀过滤即可。）

`metrics_addr` 只绑本机。要让 Prometheus 从别的机器抓，改成 `:9100` 之类，或者放到反代后面——
注意它是**无鉴权**端点，暴露出去之前先想清楚谁能访问。

### 指标清单

| 域 | 指标 | 说明 |
| --- | --- | --- |
| HTTP | `http_requests_total{method,route,status}` | `route` 是路由模板（`c.FullPath()`），不是原始 URL |
| | `http_request_duration_seconds{route}` | 请求耗时 |
| 大模型 | `llm_requests_total{model,result}` | 真正发出去的调用，成功/失败 |
| | `llm_rejected_total{reason}` | **被本地保护拦下、根本没发出去**的调用（`rate_limited` / `circuit_open`） |
| | `llm_duration_seconds{model}` | 失败也计入（否则超时会从分布里消失） |
| | `llm_tokens_total{kind}` | 服务端返回的用量；上游不回 usage 时是平的 |
| | `llm_cost_total` | **估算**的相对成本，由选窗决策累加 |
| 缓存窗口 | `llm_window_mode_total{mode}` | `extend`（吃缓存命中）/ `reset`，本项目最核心的业务指标 |
| | `llm_cache_hit_total` | 累计命中的前缀 token |
| | `cache_messages{group}` | 各群环形缓存条数（只在白名单群上产生时间线，基数有界） |
| 轮询 | `poll_cycle_duration_seconds` | 一轮拉取全部群的耗时 |
| | `poll_lag_seconds` | 最新消息从发出到被处理的延迟 |
| | `poll_fetch_errors_total{group}` | 拉取失败 |
| MCP | `mcp_tool_calls_total{server,tool,result}` | 工具调用 |
| | `mcp_sessions_up{server}` | 会话在线（1/0） |
| 协程池 | `pool_tasks_running` / `pool_queue_len` / `pool_submit_rejected_total` | 被拒绝 = 有锐评没能发出去 |
| 配置 | `config_reload_total{source}` / `config_reload_errors_total` / `config_last_reload_timestamp` | 重读失败会继续用旧快照跑，只有这里看得见 |
| 运行时 | `go_*` / `process_*` | 标准 Go / 进程指标 |

**指标命名有两条规矩**，加新指标时照做：

1. **标签值不能来自请求内容。** `route` 用路由模板；未命中路由（404）统一归成 `unmatched`。
   原始 URL 或群号直接当标签，会让基数随用户行为无限增长——这是 metrics 落地最常见的爆炸方式。
   全仓库唯一按业务 ID 打标签的是 `cache_messages{group}`，它的基数被 `allow_groups` 这个显式短名单限制住了。
2. **带标签的指标（`*Vec`）在没有任何标签组合出现过时，不会出现在 `/metrics` 里。**
   这是 Prometheus 的既定行为，不是注册失败——刚启动时看不到 `http_requests_total` 很正常。

### 看板

`docs/grafana-dashboard.json` 是一份 Grafana 看板（16 个面板：运行时概览、选窗决策、
大模型成败与拒绝、轮询延迟、MCP、协程池、配置重载）。导入方式：Grafana → Dashboards → Import，
上传这个 JSON，它会让你选一个 Prometheus 数据源（就是上面那个）。

想自己起一套的话（需要 Docker，**不是本项目的依赖**，不装也不影响任何功能）：

```bash
docker run -d --name prom -p 9090:9090 \
  -v "$PWD/docs/prometheus-scrape.yml:/etc/prometheus/prometheus.yml" prom/prometheus
docker run -d --name grafana -p 3000:3000 grafana/grafana
```

其中 `prometheus-scrape.yml` 只需两行：

```yaml
scrape_configs:
  - job_name: good-review
    static_configs:
      - targets: ["host.docker.internal:9100"]   # Linux 上换成本机 IP
```

## 链路追踪

设置 `runtime.otlp_endpoint` 为一个 OTLP/HTTP 端点即可开启（例如 `http://localhost:4318`），
改完要**重启**才生效（导出器在启动时构造）。

```bash
docker run --rm -p 16686:16686 -p 4318:4318 jaegertracing/all-in-one
```

然后访问 `http://localhost:16686` 看 Jaeger。链路形状：

```text
POST /api/debug/trigger        （HTTP 入口；span 名在路由匹配完成后被改成「方法 + 路由模板」）
  └── router.route             （一次指令路由，带 group_id / keyword / category）
        ├── llm.chat           （大模型调用，带 model / tool_count / token 用量）
        └── mcp.tools/call     （工具调用，带 server / tool）
```

轮询触发的链路没有 `http.request` 这一层，从 `router.route` 起头。

### 关闭时也有收益：日志关联

`logutil.InfoCtx / WarnCtx / ErrorCtx / DebugCtx` 会把 `trace_id` 打进日志字段。
开不开追踪，它们的成本都是一次 ctx 取值——追踪关着时 `trace_id` 字段直接不出现。

于是"这条日志属于哪次调用"这件事在两种模式下都成立：开着追踪可以在 Jaeger 里
按 trace_id 跳到完整链路，关着也能拿同一条日志里的 trace_id 去 grep 上下文。

### 为什么自己写 OTLP 导出器

官方的 `otlptracehttp` 会把 gRPC + genproto + protobuf + grpc-gateway + backoff 一整棵
依赖子树拖进来，而当前 grpc/genproto 的版本要求 `go >= 1.26`（本仓库是 1.25）。
为一棵子树长期钉死版本、或者抬主模块的 go 指令，都不该为一个**默认关闭**的功能付。

`telemetry/otlp.go` 按 OTLP/HTTP 的 JSON 编码规范手工序列化（约 150 行），
Jaeger / OTel Collector / Tempo 在 `/v1/traces` 上收的就是这个格式，协议侧没有打折。
`telemetry/telemetry_test.go` 对着 httptest 把请求体解回来逐字段核对，钉住了这件事。

代价要认：这条路径没有官方库兜底，采集端换了编码要求就得自己跟。所以那个测试不是装饰。

## 自检：这两条链路真的通吗

指标那条一条命令就够：

```bash
curl -s 127.0.0.1:9100/metrics | grep -E '^(go_goroutines|process_resident|pool_queue_len|config_last_reload)'
```

链路那条需要有个东西在收。手写导出器没有官方库兜底，所以**改完它一定要自己收一次**看
JSON 是不是对方认识的形状——单元测试只证明"符合规范"，证明不了"采集端认得"：

```bash
# 假采集端：收什么打什么
python -c "
from http.server import BaseHTTPRequestHandler, HTTPServer
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers['Content-Length']))
        print(self.path, self.headers.get('Content-Type'), body[:200])
        self.send_response(200); self.send_header('Content-Length','2'); self.end_headers(); self.wfile.write(b'{}')
    def log_message(self, *a): pass
HTTPServer(('127.0.0.1', 4318), H).serve_forever()
"
```

把 `runtime.otlp_endpoint` 指到 `http://127.0.0.1:4318`、重启、随便点几下面板，
上面就会打出 `/v1/traces application/json {...}`。启动日志里也会多一行
`链路追踪已启用 {"endpoint": ...}`——它是"我配的 endpoint 到底生效没有"的直接证据。

验收时要看的四件事（阶段 5 就是这么验的）：

1. 上报路径是 `/v1/traces`、`Content-Type` 是 `application/json`；
2. `resource.attributes` 里 `service.name=good-review`；
3. span 名是 `方法 + 路由模板`（如 `POST /api/debug/trigger`），不是原始 URL；
4. **`router.route` 的 `parentSpanId` 等于那个 HTTP span 的 `spanId`** —— 这一条才是
   "上下文真的传播过去了"。两个 span 各自存在但父子对不上，说明 ctx 在某处断了，
   链路图上就是两棵独立的树，而那正是这套埋点最容易悄悄坏掉的地方。

## 限速与熔断

出站保护（`llm.NewResilient`）包在适配器外面，配置在 `llm` 段：

| 配置 | 默认 | 作用 |
| --- | --- | --- |
| `rate_limit_per_sec` | `0`（关） | 每秒允许的调用数；超限**直接拒绝**，不排队 |
| `rate_limit_burst` | `0`（按速率取整） | 令牌桶容量 |
| `breaker_failures` | `5` | 连续失败多少次后熔断；`-1` 关闭 |
| `breaker_cooldown_sec` | `30` | 冷却后进入半开、放一个请求试探 |

MCP 工具调用另有一套同样的开关（`mcp.breaker_failures` / `mcp.breaker_cooldown_sec`，默认 5 / 60），
按**服务**分别计数——一个服务挂了不该把别的服务一起拒掉。

两处缺省不对称是刻意的：**限速默认关**（它会在正常流量下拒请求），**熔断默认开**
（只有已经连续失败 5 次它才动作，而那时每次放开调用都要等满 `llm_timeout_sec`）。
被拒绝的调用计入 `llm_rejected_total`，和被计为失败的调用分得清清楚楚。
