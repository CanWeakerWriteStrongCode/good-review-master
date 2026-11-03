# 管理面板

一个看数据的地方：谁在哪些群里、各群最近说了什么、哪里卡住了。加上登录和几个运行时开关。

## 边界

| 它做 | 它不做 |
| --- | --- |
| 登录、发 JWT、鉴权 | 发消息、改群设置（发送只走 OneBot 动作） |
| 群列表、消息、异常现场、诊断信息 | 直接读写库（全部经 `workspace` / `state` 的接口） |
| 运行时参数覆盖（写 `settings` 表） | 改 `config.yaml`（那是运维的事） |
| 托管内嵌前端 | 承载业务逻辑 |

面板和指标分成两个端口（`web_port` 17090 / `metrics_addr` 9100）：面板要过 JWT，Prometheus 抓取带不了 token；面板关掉时指标依然值得看。

## 路由

| 路径 | 鉴权 | 干什么 |
| --- | --- | --- |
| `GET /healthz` | 无 | 存活探针 |
| `GET /readyz` | 无 | 就绪探针，关闭窗口期返回 503 |
| `POST /api/login` | 有登录限流 | 换 JWT |
| `GET /api/status` | JWT | 运行状态 |
| `GET /api/workspaces` | JWT | 工作空间列表 |
| `GET /api/workspaces/:wid/groups` | JWT | 某账号的群列表 |
| `GET /api/workspaces/:wid/groups/:gid` | JWT | 某群的最近消息 |
| `GET /api/sessions` | JWT | 所有账号的会话平铺 |
| `GET /api/alerts` | JWT | 异常现场（窗口太小、重试超限等） |
| `GET /api/groups`、`GET /api/groups/:id` | JWT | 单工作空间时代的旧接口；多工作空间下返回 400，避免两个账号数据混排 |
| `GET /api/diagnostics` | JWT | goroutine 概览等运行时诊断，恒定可用 |
| `GET /api/debug/pprof/*` | JWT | CPU / heap / goroutine profile，`enable_pprof` 打开才有 |
| 其他路径 | 无 | SPA fallback，返回 `index.html` |

测试模式另有注入、重置、看状态、手动触发四类接口，注册在 `web/server/debug.go`。

## 鉴权与错误

- **JWT**。签名密钥取 `jwt_secret`；没配就退回 `web_password` 并打告警。所以改了面板密码、又没单独配 `jwt_secret` 时，所有已登录会话会一起掉线。
- **登录限流**。`/api/login` 挂了一个限流器，防止对着面板撞密码。
- **CORS**。`cors_origins` 为空时不下发任何 CORS 头（只允许同源）；填 `*` 表示放行任意来源，不推荐。
- **统一错误出口**。业务 handler 不写任何提示文案，只抛业务码；`ErrorHandler` 接住后按语言包写响应。响应体里的 `code != 200` 会记进 `http_business_errors_total`，这是面板"有接口在失败但 UI 没报错"的唯一线索。

## 前端

UniApp 编成 H5，构建产物拷到 `web/server/static/frontend/` 后用 `go:embed` 嵌进二进制。所以：

- 只拷二进制、不带前端目录也能跑，访问面板会看到"前端资源未构建"的提示页，API 仍然可用。
- 开发时跑 Vite dev server，走 proxy 打到 17090，不用每次重新构建。

改前端只改 `web/frontend`，Go 侧只负责托管和 API。

## i18n

`locales/zh-CN.yaml`、`locales/en-US.yaml` 是文案，错误码在 `errcode.go`。两者分开：加一条错误只加一个码和两条文案，不在 handler 里拼句子。

语言文件改了不用重启：`RunLocaleWatch` 轮询目录，发现变化就重载。这个 goroutine 挂在应用 ctx 上，退出时一起收。

## 关闭时的行为

`/readyz` 是给负载均衡看的。配了 `runtime.shutdown_delay_sec` 之后，进程先让 `/readyz` 返回 503，等这段时间过去再停监听——否则 LB 要等到连接被拒才反应过来，那期间的新请求会直接失败。

`/healthz` 只看进程活着，不看能否服务。

## 排错

| 现象 | 原因 |
| --- | --- |
| 打开是"前端资源未构建" | 前端产物没拷进 `web/server/static/frontend/`，跑一次 `start_main.bat` |
| 登录成功但接口全 401 | `jwt_secret` 变了、或改了 `web_password` 而没配 `jwt_secret` |
| 浏览器报跨域 | `cors_origins` 没填，或者填的源和实际访问的不一致 |
| `/readyz` 一直 503 | 还在启动，或者正在优雅关闭的等待窗口里 |
| 面板能看到群但消息为空 | 这个群的 `workspace_id` 不对（多账号时同一个物理群有两个 ID） |
| 接口 200 但 UI 报错 | 看响应体 `code`，去 `errcode.go` 查含义 |

## 代码位置

| 文件 | 看什么 |
| --- | --- |
| `web/server/server.go` | 路由注册、中间件顺序、SPA fallback、监听与超时 |
| `web/server/auth.go` | 登录、JWT 签发与校验、登录限流 |
| `web/server/middleware.go` | 鉴权、CORS、指标中间件 |
| `web/server/errors.go` / `errcode.go` | 统一错误出口与业务码 |
| `web/server/handlers.go` / `accounts.go` / `alerts.go` | 各查询接口的实现 |
| `web/server/health.go` | `/healthz`、`/readyz` 与关闭延迟 |
| `web/server/diagnostics.go` | 运行时诊断 |
| `web/server/i18n.go` / `locales_embed.go` | 语言包与热加载 |
| `web/server/embed.go` | 前端静态资源的嵌入入口 |
| `web/frontend/` | UniApp 前端源码 |
