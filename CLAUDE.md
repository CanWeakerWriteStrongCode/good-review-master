# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Verify

**没有 Makefile、没有 CI、没有统一入口**——检查直接敲命令。代价是"忘了跑"没有兜底，所以每阶段收尾请按下面这条顺序过一遍：

```bash
go build ./...        # verify all packages compile
go vet ./...          # 必须绿
go test ./...         # 必须绿
go tool golangci-lint run    # 参考，不是门禁（红了挑着修，别为它改无意义的代码）
cd tests/e2e && pnpm test    # 行为回归，每阶段收尾必跑（基线 12 过 / 2 既有失败 bot-flow、persona）
go build -o good-review-master.exe ./cmd/good-review  # 出二进制

# 只在动了热点代码 / 提示词时跑：
go test -bench . -benchmem ./cache/ ./router/   # 基准；BenchmarkAdd 的 allocs/op 必须是 0
go run ./tests/eval                             # 提示词离线评测（要真实 API Key，见 tests/eval/README.md）
curl -s 127.0.0.1:9100/metrics | head           # 指标端点自检
```

**不跑 `go test -race`**：race detector 需要 cgo + C 编译器，本机没装 gcc。
这意味着**没有任何自动并发检查**，写并发代码时靠注释里的不变量（"单写者""快照不可变"）兜着。
真需要时 `winget install BrechtSanders.WinLibs.POSIX.UCRT` 装完即可补跑。

### Go 工具（`go tool` 指令，不装全局）

三个工具用 Go 1.24+ 的 `tool` 指令钉在 `go.mod` 里（取代 `tools.go` + `//go:build tools` 那个老 hack），换机器零安装、`go mod tidy` 不会删：

```bash
go tool golangci-lint run    # 只开 govet / ineffassign / unused（见 .golangci.yml）
go tool govulncheck ./...    # 想查依赖漏洞时跑
go tool wire ./app           # 阶段 6 起：改了 provider 就要重跑，然后 git status 应无差异
```

## Testing（API 层自测）

Playwright 作为纯 API 测试运行器（`request` fixture，不启动浏览器），打真实测试二进制的 HTTP 接口。

```bash
cd tests/e2e && pnpm test
```

- 测试模式：`GOOD_REVIEW_TEST=1` 启动真实二进制 → 用 `FakeLLM` 替代真实大模型、NapCat 指向死地址（`internal/testutil/`），并注册 `/api/debug/*` 自测接口（inject/reset/state/trigger，仅测试模式可达；生产模式下这些路径只会被 SPA fallback 回成页面 HTML）。
- 用例覆盖：登录鉴权、群/消息数据、bot 全流程、缓存命中成本（扩展 vs 重置窗口）。
- 详情见 `docs/testing.md`。

## Build scripts

The frontend **must** be compiled before the Go binary — Go's `embed` resolves files at compile time. All build scripts follow this 3-step process:

| Script | Step 3 | Platform |
| --- | --- | --- |
| `build_exe.bat` | `go build -o dist\... .\cmd\good-review` (cross-compiles 4 targets) | Windows |
| `build_linux.sh` | `go build -o dist/... ./cmd/good-review` (cross-compiles 4 targets) | Linux |
| `start_main.bat` | `go run ./cmd/good-review` | Windows |
| `start_main.sh` | `go run ./cmd/good-review` | Linux |

1. `pnpm run build:h5` (in `web/frontend/`) — builds uni-app H5 frontend
2. Copy `dist/build/h5` → `web/server/static/frontend/`
3. `go build` or `go run` — **always targeting `./cmd/good-review`**, since `main.go` no longer sits at the repo root

> The repo root is **not** a main package any more. A bare `go build .` at the root compiles nothing useful. `tests/e2e/scripts/prep.mjs` and all four scripts above were updated together with that move.

**pnpm scripts** (in `web/frontend/`): `dev:h5`, `build:h5`, `dev:mp-weixin`, `build:mp-weixin`. Use `pnpm` for all package management; `npm` is blocked via a preinstall hook.

## Dependencies

| Library | Purpose |
| --- | --- |
| `github.com/sashabaranov/go-openai` | OpenAI-compatible LLM client (typed structs, connection pooling, error propagation) |
| `github.com/go-resty/resty/v2` | HTTP client for NapCat API (auto-marshal, retry, auth auto-attach) |
| `github.com/gin-gonic/gin` | HTTP framework for web management panel (routing, middleware, JSON binding) |
| `github.com/golang-jwt/jwt/v5` | JWT token signing (HS256) and validation for web auth |
| `go.uber.org/zap` | Structured logging |
| `gopkg.in/natefinch/lumberjack.v2` | Log rotation (size-based, 30-day retention, gzip compression) |
| `gopkg.in/yaml.v3` | Config YAML parsing |

## Architecture

```
QQ ←→ NapCatQQ (local HTTP API) ←→ Go bot (polling) ←→ LLM API (OpenAI-compatible)
Browser / MiniProgram ←→ Gin web server (:web_port) ←→ OneBot + Cache (read-only)
```

### Package graph

```
main → app, config, logutil, apppath, version
app → config, config/store, llm, onebot, router, bot, web/server, mcpclient, mcpserver, telemetry, logutil, internal/testutil
bot → config, cache, onebot, router, telemetry
router → config, cache, llm, onebot, async, telemetry
web/server → config, logutil, onebot, cache, version, telemetry
async → logutil, pool
pool → telemetry（仅此外只有标准库）
onebot → (no internal deps)
cache → (no internal deps)
llm → telemetry
config → apppath, logutil
config/store → logutil, telemetry
logutil → apppath, otel/trace
telemetry → (仅 prometheus / otel，无任何内部依赖)
apppath → (no internal deps)
```

`app` is the composition root (see below); `bot` is the runtime poller; `router` handles command routing with a prefix trie; `web/server` provides the web management panel (Gin + SPA); `async` provides safe goroutine launching with automatic context propagation; `onebot` is the NapCat HTTP client (resty-based) **plus CQ-code parsing** (`cqimage.go`); `cache` holds per-group zero-copy ring buffers; `llm` is the OpenAI-compatible client (go-openai SDK) wrapped in an outbound rate-limit/circuit-breaker decorator; `telemetry` owns every metric and trace; `logutil` wraps zap + lumberjack; `apppath` resolves config file paths relative to the executable.

**`telemetry` is a deliberate hole in the "no cross-package imports" rule.** It is a leaf
(only prometheus + otel), and low-level packages (`pool`, `cache`, `llm`, …) import it directly
to record metrics rather than receiving a metric handle through injection. Metrics are
process-wide singletons by nature, and threading an interface through a dozen constructors to
reach what is effectively `telemetry.PoolQueueLen.Inc()` buys no real decoupling. The trade-off
is recorded here so it is a decision rather than an accident — and so the next person knows that
`cache → (no internal deps)` still holds **because** the cache gauge is updated from
`bot/polling.go` (per group, per cycle), not from inside `cache.Add`, which must stay zero-alloc.

Two structural conventions behind that layout:

- **`main.go` lives in `cmd/good-review/`, not at the repo root.** The root is not a main package; every build/run command targets `./cmd/good-review`. `cmd/` is the conventional Go location for binaries, and it keeps the root readable as a package list.
- **CQ-code parsing lives in `onebot/`, not `cache/`.** `[CQ:image,...]` is part of the OneBot protocol, so parsing it belongs with the protocol client; `cache/` is storage and must not know about wire formats. Callers are `bot/handler.go` and `bot/polling.go` (both already import `onebot`).

### Key design: explicit dependency injection, composition root, no init() side effects

All components are constructed explicitly in the `app` package (composition root) and wired through `app.App`'s fields — nothing else constructs them. There are **zero** `init()` functions with cross-package side effects. Dependencies flow top-down through struct fields and constructor parameters. `main` only does the three things that are not wiring: logger setup, first-run prompt, and translating errors into exit codes.

`app` must not be imported by anything but `main` — when a component needs another component, inject it in `app` rather than importing across.

Known remaining global state (deliberately kept for now): `cache` package-level `cacheMap`/`anchorMap` and `logutil`'s logger are shared by `bot`/`router`/`web/server` without being held as fields.

## Startup & shutdown sequence

`main()` does only: `logutil.SetupLogger()` → `config.InitDefaultFiles()` (first run: create templates, prompt, exit 0) → `app.New(...)` → `app.Run()`.

**`app.New(opts)` — assembly** (see `app/build.go`; the only place components are constructed):

1. `config.Load(config.Sources{Config, Secret})` (config + secret + validation, see *Config layering* below) → `config.LoadPromptConfig` → LLM client (`testutil.FakeLLM` when `GOOD_REVIEW_TEST=1`, else provider switch). All failures return before any resource is taken; `Load` is the only place that decides whether the config is usable, so `New` does not re-check anything.
2. `signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)` — the app's lifecycle context, owned by `App`.
3. `onebot.NewClient` — OneBot HTTP client (resty).
4. `obClient.GetLoginInfo()` — bot nickname (failure is non-fatal). **Must come before the snapshot is built**, so the nickname is baked into an immutable config instead of being written into it later.
5. Built-in `view_image` MCP server when `llm.image_max > 0` (binds 127.0.0.1); its address is remembered on `App` for `derive` to inject. Failure is non-fatal.
6. **`derive` + first snapshot**: `store.NewStore(initial)`; `App.Config` is the store's `Get` method value. Nothing mutates this config afterwards.
7. `mcpclient.New(initial.MCPConfig, ctx)` + `LogConfig()` + `Start()` — uses the initial config, not the snapshot: MCP servers do **not** hot-reload.
8. `router.NewRouter(snapshot, promptCfg, llm, ob, mcpMgr, ctx)` — router receives the lifecycle context for its goroutine group.
9. Startup logs; `bot.NewBot(snapshot, ...)`; `webserver.New(snapshot, obClient)` + `EnableDebug(router, fakeLLM)` when test mode (conditional, `initial.WebPort > 0`).
10. `store.NewInformer(configStore, list, FilePoller)` + `store.NewObserver(promptPoller, reloadPrompts)` — built last, both started by `Start()`. The prompt observer calls `Prompt.Reload()` then `Router.Rebuild()`.

**`app.Run()` — lifecycle** (`app/app.go`): `Start()` launches the polling goroutine, the config informer, and the web server (all non-blocking) → blocks on `ctx.Done()` → `Shutdown(ctx)` closes in order: cancel ctx → `web.Shutdown` (10s timeout) → `router.Wait()` → `mcpMgr.Close()` → built-in MCP `Close()`. `Shutdown` is idempotent (`sync.Once`); shutdown failures are logged only and do not change the exit code. The informer exits on the same cancelled ctx — if it didn't, it would hold up graceful shutdown.

**Readiness during shutdown (invariant):** `webserver.Server.Shutdown` sets the `draining` flag **first**, then optionally waits `shutdown_delay_sec`, and only then stops the listener. The order is essential and was empirically verified: `http.Server.Shutdown` closes the listener *immediately*, so setting the flag and calling it back-to-back leaves `/readyz` no observable window — a probe cannot even open a connection (it gets an RST). The wait is what lets a load balancer notice the 503 and drain. With the default `shutdown_delay_sec: 0` there is no observable, which is correct for a directly-reached panel.

**MCP session lifetime (invariant):** `mcpclient.Manager`'s lifetime deliberately uses `context.WithoutCancel`, so sessions and their stdio subprocesses end **only via `Manager.Close()`** — the graceful path (close stdin → child exits on its own) never gets a chance if a parent context cancellation kills the child first. `Close()` closes sessions *before* cancelling the lifetime, and takes a `closing` gate so the reconnect loop can't spawn a new subprocess mid-shutdown. Any exit path that skips `App.Shutdown` leaks stdio subprocesses. In `Close`, a child's non-zero exit code is only a WARN, not a failure: servers commonly treat stdin EOF as an error (the Go SDK's own server returns `server is closing: EOF`).

## Config files

| File | Loaded by | Hot-reload |
| --- | --- | --- |
| `config.yaml` | `config.Load(config.Sources{...})` | Yes (snapshot swap) |
| `secret.yaml` | same `Load` call (`Sources.Secret`) | Yes (snapshot swap) |
| `prompt_system.yaml` | `config.LoadPromptConfig()` | Yes (`PromptConfig.Reload()`) |
| `prompt_custom.yaml` | merged into `PromptConfig` at startup | Yes (`PromptConfig.Reload()`) |

All four YAML files are auto-created from embedded templates on first run if missing (`config.InitDefaultFiles()`, the only place allowed to create files). `config.yaml`, `secret.yaml` and `prompt_system.yaml` use templates under `config/`; `prompt_custom.yaml` is created empty on first keyword addition.

`config.yaml` has five sections: `napcat`, `bot`, `runtime`, `llm`, `mcp`. Prompt files have `cmd:` (map of category → list of `{keyword, prompt}`) and `rules:` (map of category → shared rules string appended to every prompt of that category).

`prompt_system.yaml` is parsed once at startup and cached (`Config.systemPrompt`) — subsequent checks read the cached pointer, not the file. `prompt_custom.yaml` is read on every write operation (add/delete command/rule) and on `Reload()`.

### Config layering: Sections, registry, four-step load

The config layer follows the *shape* of K8s' API machinery (external/internal split + a registry) without importing any of it. One "domain" = one file:

| File | Role |
| --- | --- |
| `config/section.go` | the `Section` interface + shared helpers (`decodeSection`, `validateEndpoint`, …) |
| `config/scheme.go` | **the registry**: `newSections()` + `registry()`. Adding a domain = one file + one line |
| `config/section_{napcat,bot,runtime,llm,mcp}.go` | each holds its own yaml tags, defaults, and validation |
| `config/section_secret.go` | the secret domain — **sources differ**, see below |
| `config/loader.go` | the four-step load: read → decode → SetDefaults → Validate |
| `config/convert.go` | external → internal (`assemble`), plus secret precedence |
| `config/config.go` | the internal `Config`/`LLMConf`/`MCPConf`/`MCPServerConf` types — **shape unchanged by the refactor** |

Why the internal shape is frozen: `mcpclient`, `router` and `web/server` tests construct `config.Config{...}` / `config.MCPConf{...}` literals directly. Consumers still write `cfg.BotQQ`.

**Ordering is load-bearing.** `SetDefaults` must run *before* `Validate`, because a field that is legitimately omitted must be defaulted and then validated — not rejected. Note that several numeric checks are only meaningful post-default, and they are not decoration: `poll_interval_sec` feeds `time.NewTicker` (**panics** if ≤ 0, inside a goroutine, taking the process down), `llm_timeout_sec` feeds `context.WithTimeout` (= 0 means every LLM call fails instantly), `max_cache_msg` feeds `make([]Message, n)` and is then indexed.

**`Load` must stay side-effect free and idempotent.** Scheduled work will call it repeatedly (config reload + resync), so `InitDefaultFiles` (which creates files) must never be called from it. `config/loader_test.go` asserts both properties.

**Secret domain** is deliberately outside `registry()`: its sources are `secret.yaml` + environment variables, not `config.yaml`, and `secret.yaml`'s whole content *is* the domain (no top-level key to look up). Precedence is **env > secret.yaml > config.yaml legacy location**; env is folded into the domain during decode, so `convert.go` only has to resolve two layers. Legacy values still work and emit a migration WARN — the program never rewrites the user's files.

Per-domain `Validate` handles single-domain rules; cross-domain rules (e.g. `web_port > 0` requires credentials, which may live in `secret.yaml`) live in `Config.Validate`, which runs at the end of `Load`. Validation errors are written for whoever edits the YAML: name the field, show the current value, say what to write instead. Never let a struct-tag validator produce messages like `Field validation for 'X' failed on the 'gt' tag`.

`secret.yaml` is created with mode `0600` — **but that is a no-op on Windows**, where Go's file modes only map to the read-only attribute. On Windows its protection comes from filesystem ACLs; on Linux/macOS the mode does apply.

### Config snapshots (`config.Snapshot` + `config/store/`)

Consumers no longer hold a `*config.Config`. They hold a `config.Snapshot`, which is `func() *Config`:

```go
cfg := b.cfg()        // ← take ONE snapshot per operation, use cfg.X throughout
```

**Why a function and not a pointer:** a struct field of type `*config.Config` invites the reader to treat it as a constant ("it was loaded at startup"). Hot-reload then silently does nothing — no error, just stale values. Writing `cfg()` every time makes the mutation visible at the call site.

**Take one snapshot per operation, never several.** Each accessor calls it once at the top and threads the result down (`ProcessMessage` → `isAtBot`, `RouteMessage` → `stripCQPrefix`, `chatReview` → `selectChatWindow`). Calling `cfg()` repeatedly inside one operation means a reload landing mid-flight can produce a mixed state — e.g. a system prompt with the new model name and the old nickname.

| Piece | File | Role |
| --- | --- | --- |
| `Snapshot` | `config/snapshot.go` | `func() *Config`; satisfied directly by `store.Store.Get`'s method value, so `config` and `config/store` need not import each other |
| `Store[T]` | `config/store/store.go` | immutable snapshot behind `atomic.Pointer`; readers always see one self-consistent version |
| `Source` | `config/store/source.go` | emits "changed" signals; `FilePoller` stats files (mtime+size) every 2s |
| `Informer[T]` | `config/store/informer.go` | List → swap → notify handlers; runs under `app`'s lifecycle context |
| `Observer` | `config/store/observer.go` | "changed → call back", **without** holding a snapshot |

`Informer` and `Observer` exist as two shapes rather than one with a flag, because they serve data with different owners. `Informer` is for data that has **no** snapshot yet (the app config); `Observer` is for data that **already** owns and atomically publishes its own snapshot (the prompt config). Forcing the second case through `Informer` would create a second source of truth for the same data. Both share `Source` — the part that is genuinely common — and each keeps its own small loop.

### What hot-reloads, and what does not

| Changed | Takes effect |
| --- | --- |
| `prompt_system.yaml` / `prompt_custom.yaml` | within ~2s (`PromptConfig.Reload()` **and** `Router.Rebuild()`) |
| `allow_groups`, thresholds, cost params, model name, `max_msg_rune`, poll interval | on the next read (next tick / next request) |
| `web_port`, credentials, `jwt_secret`, `mcp.servers`, `metrics_addr`, `otlp_endpoint`, `llm.rate_limit_*`, `*.breaker_*` | **restart required** — a WARN says so on reload |

The limiter and breaker are **stateful** (a token bucket, a consecutive-failure counter), so
re-reading their numbers mid-flight would silently change what the counters mean. That's why
they belong in the same category as the bound listener — not because they're expensive to rebuild.

The prompt path needs *both* steps: `Reload` only swaps the data, while the route table is **derived** from it. Skipping `Rebuild` yields the worst kind of bug — the file clearly contains the new keyword, and nothing happens when you use it.

The restart-required list is not arbitrary: those values are consumed at construction (listener bound, auth middleware installed, MCP clients built). `warnRestartOnlyChanges` compares the old and new snapshots and reports them, because once hot-reload exists users reasonably assume *everything* is live and will otherwise waste time wondering why changing the port did nothing.

**`PromptConfig` publishes once, never mutates.** `Reload` builds entirely fresh maps and stores them via `atomic.Pointer[PromptData]`; readers get a map nobody will ever write to again. This is not stylistic — Go's concurrent map read/write is a **fatal error** (`concurrent map read and map write`), not a race warning, and hot-reload puts `Reload` on the informer goroutine while dispatch and the internal-command tasks read the same data. The old `load()` published `pc.CmdConfigs` and *then* mutated it while merging the custom file, which was a live race. `config/prompt_test.go` pins the immutability.

`PromptConfig.Snapshot()` returns the merged result; `getSystemPrompt()` returns the system file *alone*. The guard functions (`KeywordInSystemCmd`, `CategoryInSystemRule`) must use the latter: asking "does this keyword belong to the system?" against the merged data would misclassify user-added commands as system ones, making them undeletable.

**A file source has no deltas.** Files are rewritten wholesale, so every change is a full re-List and the snapshot is swapped entire — there is no merge step, which is why `old`/`new` handed to handlers are always two complete, self-consistent configs.

**On a failed re-read the old snapshot is kept.** Users edit config halfway all the time; continuing with the last working config beats being pushed into a "no config" state. There is a test for this.

**`app.derive` is not optional on the reload path.** The nickname (from NapCat) and the built-in `builtin_image` MCP address are not in any file, so a config re-read from disk *cannot* contain them. Both the initial snapshot and the informer's `list` go through `derive`; skipping it would silently drop them on the next reload (visible only as "@-detection stopped working" or "view_image disappeared"). `app/derive_test.go` pins this, including the copy-before-append so the new snapshot never shares a backing array with the published old one.

The initial snapshot is built **after** the nickname is fetched and the built-in MCP is started — that ordering is what makes "snapshots are immutable" true from the very first one, replacing the two runtime mutations the old code did (`cfg.BotNickname = ...` and injecting into `cfg.MCPConfig`).

What hot-reloads and what does not: `allow_groups`, thresholds, cost params, model name take effect on the next read. `web_port`, credentials, `jwt_secret` and `mcp.servers` are **startup-time** decisions (listener bound, middleware installed, clients constructed) and need a restart. `webserver.New` takes one startup snapshot for exactly those, while `/api/status` and `/api/groups` call the snapshot per request.

## Command system (`router/`)

### Two kinds of commands

| Kind | Defined in | Examples |
| --- | --- | --- |
| Internal | `router/internal_cmd.go` via `Router.register()` | `添加关键字`, `删除关键字`, `帮助` |
| User | `prompt_system.yaml` / `prompt_custom.yaml` YAML lists | `锐评下`, `猫娘` |

### Router struct (`router/command.go`)

```go
type routeTable struct {          // 一份完整路由表快照，发布后不可变
    trie   *trieNode              // 前缀树匹配，O(k)
    routes []Command              // 帮助列表遍历
}
type Router struct {
    table            atomic.Pointer[routeTable] // 整体替换，分发时无锁读取
    internalCommands []Command                  // 内部指令，仅启动时注册
    handlerMap       map[string]HandlerFunc
    llmClient        llm.Client
    obClient         *onebot.Client
    promptCfg        *config.PromptConfig
    appCfg           config.Snapshot
    starter          *async.Group               // goroutine 生命周期管理
}
func NewRouter(appCfg, promptCfg, llmClient, obClient, shutdownCtx) *Router
func (r *Router) RouteMessage(ctx, content, event, groupID)
func (r *Router) Rebuild()                            // 提示词变了之后重建路由表
func (r *Router) Go(fn func(context.Context) error)   // 安全启动 goroutine
func (r *Router) GoCtx(parent, fn)                    // 同上，但继承调用方 ctx（trace 靠它跨 goroutine）
func (r *Router) Wait() error                         // 等待所有 goroutine 退出
```

`RouteMessage` 收 ctx、`HandlerFunc` 的第一个参数也是 ctx：分发链路的上下文要一路传到
处理器里的大模型与 MCP 调用，否则一次"消息 → 路由 → 大模型 → 工具"在链路图上是散的。
`chatReview` 因此用 `GoCtx`（而不是 `Go`）提交异步任务——见 `async.Group.GoCtx` 的说明。

`trie` and `routes` live in **one** atomically-swapped struct on purpose: with two separate atomics, a dispatch could pair a new trie with an old help list, so what `帮助` lists and what actually matches would disagree — the kind of inconsistency that is miserable to debug. `Rebuild` needs no lock: it builds a fresh table and stores it, so concurrent rebuilds converge and readers never see half a table.

### Route matching: prefix trie

Routes are stored in a prefix trie (`trieNode`), NOT a flat slice. Matching walks the trie character by character and returns the **longest matching prefix** — e.g., "锐评下" matches before "锐评".

```go
func trieMatch(root *trieNode, text string) *Route   // O(k), k = len(text)
```

`rebuild()` iterates all routes and inserts them into the trie. A flat `[]Route` is also maintained for the `帮助` command to list all user commands.

### Route dispatch (`Router.RouteMessage()`)

1. `stripCQPrefix()` — strips `[CQ:at,qq=xxx]` codes and `@Nickname` text
2. `trieMatch()` — longest prefix match on cleaned text
3. `ComposeSystemPrompt()` builds the system prompt (bot identity + optional image hint + optional persona block)
4. `BuildUserMsg()` builds the user message (chat log + fixed instruction + the keyword's prompt)
5. Handler receives `(ctx, event, groupID, systemPrompt, keywordPrompt, mentionerNick, extra)`

> ⚠️ **文档与代码的一处已知不符（阶段性 5 发现，未修）**：`extra`（关键字后面的补充文本）
> 目前**没有**被拼进提示词——它一路传到处理器，但 `BuildUserMsg` 从未使用过它
> （`mentionerNick` 同样）。早先的文档把它写成"`用户补充,优先级很高:{extra}` 追加"，
> 那是更早版本的行为。这是一处需要产品决策的回归：把它加回去会改变发给大模型的内容，
> 所以阶段 5 只删掉了两个死参数、没有擅自恢复。`tests/eval` 走的是同一对函数，
> 恢复时评测会自动跟着变。

### Message flow

```
polling (bot/polling.go) → fetch history (onebot.Client)
                         → dedup via cache.HasMsgID (O(1) map)
                         → ProcessMessage (bot/handler.go)
                            → whitelist check (Config.HasGroup)
                            → truncate to MaxMsgRune
                            → add to ring cache (zero-copy)
                            → @bot detection (QQ number + nickname)
                            → router.RouteMessage → handler
```

## Adding a new command type

1. Write a handler method: `func (r *Router) handlerName(event onebot.Event, groupID string, prompt string)`
2. Add to `handlerMap` in `router/command.go` `NewRouter()`: `"category_name": r.handlerName`
3. Add entries in `prompt_system.yaml` under `cmd.category_name:` as a list of `{keyword, prompt}`
4. Optionally add shared rules under `rules.category_name:`

Routes are auto-generated. No trie changes needed.

## Internal commands

Defined purely in Go (no YAML). Registered in `registerInternalCommands()` called from `NewRouter()`. Currently five: add keyword (prompt via LLM), delete keyword, add rule, delete rule, help listing.

`添加关键字` format: `添加关键字(关键词)指令(指令类型)大模型想提示词(要点)` — the LLM generates the prompt from the requirements. Writing goes to `prompt_custom.yaml`.

`删除关键字` format: `删除关键字(关键词)`. Both refuse to touch keywords that exist in `prompt_system.yaml` or in the registry.

Guard checks use `promptCfg.KeywordInSystemCmd(keyword)` and `CategoryInSystemRule(category)` — both read from the cached system prompt, not from disk.

## @mention detection (`bot/handler.go`)

`Bot.isAtBot(rawMsg)` checks two things: `strings.Contains(rawMsg, b.cfg.BotQQ)` (catches CQ codes like `[CQ:at,qq=xxx]`), and `strings.Contains(rawMsg, "@"+b.cfg.BotNickname)` (catches text @mentions). The nickname is fetched at startup via `onebot.Client.GetLoginInfo()`; failure is non-fatal.

## LLM client (`llm/`)

```go
type Client interface {
    Review(ctx context.Context, chatLog, systemPrompt string) (string, error)
}
```

`OpenAIAdapter` implements `Client` using the `go-openai` SDK. Benefits over previous custom HTTP: shared `http.Client` (connection pooling), typed request/response structs (no `map[string]any`), proper error propagation (no discarded marshal errors), HTTP status code checking, retry support built into the SDK. The `Client` interface is preserved — callers unchanged.

## OneBot client (`onebot/`)

```go
type Client struct { /* unexported: httpAPI, accessToken, restyClient */ }
func NewClient(httpAPI, accessToken string) *Client
func (ob *Client) GetLoginInfo() (*LoginInfo, error)
func (ob *Client) GetGroupInfo(groupID string) (*GroupInfo, error)
func (ob *Client) SendGroupMessage(groupID, content string)
func (ob *Client) FetchGroupMsgHistory(groupID string, count int) ([]HistoryMsg, error)
```

Uses `resty` — Base URL, auth token, and Content-Type set once in `NewClient()`. All methods use `SetBody()` + `SetResult()` for automatic JSON marshal/unmarshal. Built-in retry (2 attempts). No repeated boilerplate per endpoint. No dependency on `config` package.

## Web Management Panel (`web/server/` + `web/frontend/`)

Gin-based HTTP server + uni-app Vue 3 SPA, embedded into the Go binary via `//go:embed`.

### Backend (`web/server/`)

| File | Role |
| --- | --- |
| `server.go` | Gin engine, route registration, SPA fallback, graceful shutdown |
| `handlers.go` | API handlers: login, logout, status, groups list, group messages |
| `auth.go` | JWT generation (HS256, 24h expiry) and parsing; login failure rate limit (per-IP token bucket, failures only) |
| `middleware.go` | Logger, Recovery (panic guard), CORS (allowlist), JWT auth guard |
| `health.go` | `/healthz` + `/readyz` probes (no auth) |
| `diagnostics.go` | `/api/diagnostics` — runtime summary + goroutine grouping (always on, parsed in Go) |
| `pprof.go` | `net/http/pprof` under the authenticated `/api/debug/pprof/`, off unless `runtime.enable_pprof` |
| `embed.go` | `//go:embed static/frontend` |

**API endpoints:**

| Method | Path | Auth | Description |
| --- | --- | --- | --- |
| POST | `/api/login` | No | Returns JWT token (validates username/password; rate-limited) |
| GET | `/api/status` | JWT | BotQQ, Nickname, MaskedAPIKey, GroupCount |
| GET | `/api/groups` | JWT | Per-group info with activity stats |
| GET | `/api/groups/:id` | JWT | Cached messages for one group |
| POST | `/api/logout` | JWT | No-op (stateless token) |
| GET | `/api/diagnostics` | JWT | Runtime summary + goroutine grouping; `?full=1` adds the stack dump |
| GET | `/healthz` | **No** | Liveness: always 200 while the process serves |
| GET | `/readyz` | **No** | Readiness: 503 while draining (see `shutdown_delay_sec`) |
| GET | `/api/debug/pprof/*` | JWT | Only when `runtime.enable_pprof: true` |

Key details: Gin runs in ReleaseMode; `web_password` is required (no password-less mode — the bot exits at startup if web is enabled without a password); `groupNames` map caches GetGroupInfo results to avoid repeated NapCat calls.

**Security posture** (all changed together — keep them consistent if you touch auth):

- **Login rate limit** — per-IP token bucket, 5 consecutive failures then 429 (`Retry-After: 60`). Only *failures* consume tokens, so a successful login never throttles a legitimate user. `engine.SetTrustedProxies(nil)` is set deliberately: gin trusts `X-Forwarded-For` by default, which would make `c.ClientIP()` attacker-controlled and the limit trivially bypassable. If you ever put this behind a reverse proxy, change it to `SetTrustedProxies([]string{"<proxy-ip>"})` — never re-open it to all.
- **CORS** — default is same-origin only (no CORS headers at all). `runtime.cors_origins` adds an allowlist; `*` re-opens it to everything. The dev Vite server uses a proxy, so nothing needs configuring.
- **JWT** — signs and verifies HS256 only. `jwt_secret` is separate from `web_password`; if unset it falls back to the password with a WARN (note: jwt/v5 already rejects `alg=none` and RS256 key-confusion via its keyfunc, so `WithValidMethods` is defense-in-depth, not a fix for a live hole).
- **Probes leak nothing** — `/healthz` and `/readyz` are unauthenticated, so their bodies contain status strings only: never config values, group IDs or upstream addresses. They also deliberately **do not** check NapCat reachability: that is degradation, not unreadiness, and an unauthenticated endpoint must not trigger outbound calls.
- **Diagnostics are JWT-gated** — `/api/diagnostics` and `/api/debug/pprof/*` both sit behind `AuthMiddleware`. pprof can dump heap contents and call stacks (including config values), so it is never a public endpoint.

### Diagnostics page (`pages/diagnostics/index.vue`)

An authenticated panel page showing runtime health. Reachable via the 「诊断」 button in the groups page status bar (there is no central nav component).

**The split that matters:** goroutine analysis does **not** depend on `runtime.enable_pprof`.

| Section | Source | Needs `enable_pprof` |
| --- | --- | --- |
| Overview cards + goroutine grouping + full stack | `GET /api/diagnostics` | no |
| heap / allocs / block / mutex / threadcreate text | `GET /api/debug/pprof/<name>?debug=1` | **yes** |
| Binary profile download (for `go tool pprof`) | `GET /api/debug/pprof/<name>` | **yes** |

Reasons for that split: `enable_pprof` carries real sampling overhead and defaults to off, while diagnostics should always be available; and `runtime.Stack(buf, true)` yields the *same* data as pprof's `goroutine?debug=2`, so taking it directly costs nothing extra. Parsing the dump lives in Go (`diagnostics.go`), where it is unit-tested — the frontend has no test framework, so a fragile text parser must not live there.

`groupGoroutines` keys each goroutine by the **first frame that is not `runtime.`/`runtime/`/`sync.`/`internal/`/`os/signal.`**. Skipping those is the whole point: parked goroutines all sit in `runtime.gopark`, so without skipping you get one giant bucket. Two subtleties are load-bearing and both were caught by tests, not by reading:

1. **Frame names must have their argument list stripped.** Arg values contain pointers, so `net/http.(*Server).Serve(0xc000..., {...})` differs per goroutine — keeping args makes every goroutine its own group and the aggregation silently does nothing.
2. **`%2e` must be unescaped.** The compiler escapes `.` in the last segment of a package path (`cmd/internal/objabi/path.go`), so `gopkg.in/natefinch/lumberjack.v2` appears as `lumberjack%2ev2`. Only `%2e` needs replacing, because a real `%` is escaped to `%25`.

The dump is fetched only with `?full=1` (auto-refresh must not ship megabytes), rendered truncated at 256 KB, and the page's auto-refresh defaults to **off** because `runtime.Stack(all=true)` stops the world.

The frontend streams non-envelope responses through `src/api/diagnostics.ts`, which bypasses `request<T>` (that helper only unwraps `{code,data}`). Two verified uni-app facts it depends on: `responseType` accepts only `'text'` or `'arraybuffer'` (anything else is *silently* downgraded to text), and plain text needs no special option because `parseResponseText` try/catches `JSON.parse` and returns the raw string. Downloads are the repo's **first H5-only code** (`Blob` + `createObjectURL` + a synthetic `<a download>`), guarded with `#ifdef H5`; MiniProgram gets a toast instead.

### Frontend (`web/frontend/`)

uni-app Vue 3 project — targets H5 web and WeChat MiniProgram. 4 pages: Login, Groups List, Message Detail, Diagnostics. Pinia stores with token in `localStorage["good_review_token"]`. Hash routing for SPA compatibility.

Conventions worth matching: no UI library (hand-rolled `<view>`/`<text>` + scoped plain CSS, navy gradient `#1a1a2e → #16213e` header motif, white cards); always `<script setup lang="ts">`; each protected page runs its own auth guard in `onMounted` (`authStore.isAuthenticated()` else `uni.reLaunch` to login) — there is no router guard or shared nav component. Registering a page means a `.vue` file plus one entry in `src/pages.json`; nothing else (no tabBar, no route table). `api/index.ts` exports `request<T>` (unwraps `{code,data}`), `getAuthHeader()` and `handleUnauthorized()` — reuse the latter two anywhere you must call `uni.request` directly.

**Build requirement:** Frontend must be built before the Go binary. Running `go build` without a pre-built frontend produces a working bot but the web panel returns "前端资源未构建".

**Vite plugin `go-embed-fix`:** Go's `embed` skips files starting with `_` or `.`. uni-app H5 plugin generates chunk files with `_` prefix; this custom plugin renames them to `chunk-` prefix so they pass Go's embed filter.

## Ring buffer cache (`cache/`)

Per-group `GroupMsgCache` — **true ring buffer with zero-copy writes**:

```go
type GroupMsgCache struct {
    buf      []Message             // 固定大小，只分配一次
    writeAt  int                   // 写指针，满了循环覆盖
    msgIDSet map[int64]struct{}    // O(1) 去重
    filled   bool                  // 是否已写满一圈
    mu       sync.RWMutex
}
```

`Add()`: writes at `writeAt`, overwrites oldest if full, advances pointer — never copies the buffer. `GetAll()`: reorders `[writeAt, end)` + `[0, writeAt)` into time-ordered copy. `HasMsgID()`: O(1) map lookup. For n≈20 messages, the two-segment copy in GetAll is negligible.

Single-writer architecture (only the polling goroutine calls `Add`) — no lock contention in practice.

**`Add` must stay allocation-free, and `cache/bench_test.go` enforces it.** `BenchmarkAdd`'s
`allocs/op` is 0 — that number *is* the "zero-copy" claim made executable. Anything that escapes
to the heap on that path (a metrics call, a log field, a closure) turns it into 1 and the
benchmark says so. The `cache_messages` gauge is therefore updated from `bot/polling.go`
(per group, per cycle), never from inside `Add`.

**Global functions used by the web API:**

```go
func ListGroupIDs() []string                         // all cached group IDs
func GetCache(groupID string) *GroupMsgCache         // nil if not cached
func GetGroupCache(groupID, maxSize int) *GroupMsgCache  // get or create
func BuildChatLog(msgs []Message) string             // format as chat context text
func (gc *GroupMsgCache) Len() int                   // current message count
```

## Safe goroutine management (`async/` + `pool/`)

`async` 基于自定义协程池（`pool`）提供安全 goroutine 管理，不再依赖 `golang.org/x/sync/errgroup`。

### Pool (`pool/pool.go`) — 通用协程池

```go
type Pool struct { /* chan + sync.WaitGroup */ }
func New(size int) *Pool          // size<=0 时默认 runtime.NumCPU()*2
func (p *Pool) Submit(task func()) bool  // 非阻塞提交，队列满返回 false
func (p *Pool) Shutdown()                // 优雅关闭：停止接收，排空队列
```

纯工具包，仅依赖标准库 `sync`。Worker 固定数量，有界任务队列。`Submit` 非阻塞，背压由上层 `async` 处理。

### async (`async/async.go`) — 安全执行层

```go
type Group struct { /* pool + ctx + cancel */ }
func New(ctx context.Context) *Group
func (g *Group) Go(fn func(context.Context) error)  // auto ctx + panic recover
func (g *Group) Wait() error
```

基于 `pool` 封装，提供：context 自动传递、panic recover + 日志、阻塞式任务提交（队列满时等待或取消）。`Router` 持有 `*async.Group` 并暴露 `Go(fn)` / `Wait()` 代理方法。Handler 通过 `r.Go(func(ctx) ...)` 提交异步任务 —— ctx 自动从 shutdown context 派生，Ctrl+C 可取消进行中的 LLM 调用。

## Observability (`telemetry/`)

指标与链路同住一个包，因为它们的生命周期完全一致：同一份配置开关、同一次初始化、同一次 Shutdown。

```go
telemetry.NewRegistry()                                  // 私有注册表（不用 Prometheus 全局单例）
telemetry.NewMetricsServer(addr, registry)               // /metrics 独立监听，默认 127.0.0.1:9100
telemetry.InitTracing(endpoint, serviceName) (stop, err) // 空 endpoint = 关闭（默认）
telemetry.StartSpan(ctx, name, opts...)                  // 关闭时是 no-op，调用点不必判开关
```

**`/metrics` 独立监听、不挂业务端口**：面板要过 JWT 而 Prometheus 抓取带不了，而且
`web_port<=0` 时本进程依然是一个在跑的机器人，依然值得被观测。绑不上端口只记一条错误，
不拦启动——指标是附加能力。

**标签基数两条硬规矩**（见 `telemetry/metrics.go` 的 `HTTPRouteLabel`）：
`route` 用 `c.FullPath()` 路由模板、未命中路由统一归 `unmatched`，**绝不用原始 URL**
（`/api/groups/<群号>` 会随群号数量无限增长）；全仓库唯一按业务 ID 打标签的是
`cache_messages{group}`，其基数被 `allow_groups` 这个显式短名单限制住。

**`llm_rejected_total` 与 `llm_requests_total` 的分工**：前者是"被本地限速/熔断拦下、
根本没发出去"的调用。只看 `result=error` 会把熔断期间算成"没有请求"，
看起来像流量凭空消失。排障时两条要一起看。

**OTLP 导出器是自己写的**（`telemetry/otlp.go`，约 150 行）。官方的 `otlptracehttp` 会拖进
gRPC + genproto + protobuf + grpc-gateway + backoff 一整棵子树，而当前 grpc/genproto 要求
`go >= 1.26`（本仓库是 1.25）。为一个**默认关闭**的功能长期钉死一棵依赖、或抬高主模块
go 指令，都不划算。OTLP/HTTP 的 JSON 编码是规范定义的标准形态，Jaeger / Collector / Tempo
在 `/v1/traces` 上收的就是它，所以协议侧没有打折——`telemetry_test.go` 对着 httptest
把请求体解回来逐字段核对（traceId 32 位十六进制、int64 用字符串、**status 码的两套枚举顺序相反**）。

**日志关联是追踪在"没有后端"时也成立的收益**：`logutil.InfoCtx(ctx, ...)` 一族会把
`trace_id` 打进日志字段；追踪关着时该字段直接不出现，成本是一次 ctx 取值。

**出站保护（限速 + 熔断）** 是包在 `llm.Client` 外面的装饰器（`llm.NewResilient`），
不塞进适配器内部——"要不要发这一趟"与"怎么发"是两件事。缺省刻意不对称：
**限速默认关**（它会在正常流量下拒请求），**熔断默认开**（只有已经连续失败 5 次才动作，
而那时每次放开调用都要等满 `llm_timeout_sec`）。`context.Canceled` 通过 gobreaker 的
`IsExcluded` 排除在外——否则每次 Ctrl+C 都会给熔断器记一笔失败，重启后头几个请求
可能直接撞上"熔断中"，一个纯粹由正常操作制造出来的假故障。MCP 侧按**服务**分别熔断。

## Logging (`logutil/`)

Uses `zap` + `lumberjack`. Dual output: console (colored) + file. `lumberjack` handles rotation: 20MB max, 30 backups, 30-day retention, gzip compression. `zap.AddCallerSkip(1)` makes caller field point to actual call site, not the `logutil` wrapper.

Thin wrappers: `Info(msg, kv...)`, `Error(msg, kv...)`, `Warn(msg, kv...)`, `Debug(msg, kv...)` — all delegate to `sugar.Infow/Errorw/Warnw/Debugw`.

## Config notes

- `config.yaml`, `secret.yaml` and `prompt_custom.yaml` contain real credentials/user data — NOT committed (`.gitignore` covers all four)
- `prompt_system.yaml` also NOT committed (auto-created from embedded `config/prompt_system_example.yaml` on first run, like config.yaml)
- `config_example.yaml` is the committed template, embedded via `//go:embed` and auto-copied to `config.yaml` on first run
- On first run, `config.yaml` is created from the embedded template and the program exits — edit it and re-run
- `prompt_system.yaml` is also auto-created with a comment header if missing
- `runtime.web_port` — web panel port, <=0 disables the web server
- `runtime.web_username` / `runtime.web_password` — login credentials (both required when the web panel is enabled)
- `runtime.jwt_secret` — JWT signing key; falls back to `web_password` with a WARN when empty (a password change then invalidates every logged-in session)
- `runtime.cors_origins` — comma-separated origin allowlist; empty = same-origin only. `*` allows any origin
- `runtime.enable_pprof` — exposes `net/http/pprof` under the authenticated `/api/debug/pprof/`
- `runtime.shutdown_delay_sec` — pre-stop wait: mark not-ready, wait this long, *then* stop the listener. Default 0 (stop immediately). Only meaningful behind an LB / k8s / nginx that polls `/readyz`; must stay under the 10s shutdown budget in `app.Run`
- `runtime.metrics_addr` — `/metrics` listener. **Defaults to `127.0.0.1:9100`** (loopback only); `off`/`none`/`disabled` turns it off. Empty means "use the default", which is why an explicit sentinel is needed. Restart required
- `runtime.otlp_endpoint` — OTLP/HTTP traces endpoint (e.g. `http://localhost:4318`). Empty = tracing off (default). Restart required
- `llm.rate_limit_per_sec` / `llm.rate_limit_burst` — outbound rate limit; default 0 = off. Over-limit calls are **rejected**, not queued
- `llm.breaker_failures` / `llm.breaker_cooldown_sec` — circuit breaker; defaults 5 / 30s, `-1` disables. Same pair under `mcp.` (defaults 5 / 60s, counted **per server**)
- `mcp.retry_interval_sec` / `llm.breaker_failures` use `-1` as the "explicitly disabled" sentinel — `0` means "unset, use the default". Same idiom, keep it consistent
- `Config.MaskedAPIKey()` — returns API key with only first 4 and last 4 chars visible (e.g., `sk-9a****d8`), used by web API
- `apppath.ResolvePath(filename)` searches `./` then `exeDir/`
- `config.CustomPromptPath(systemPath)` gives `prompt_custom.yaml` path in the same directory as `prompt_system.yaml`

## Code conventions

- **Variable naming**: 变量名不要简化，要完整让人方便看懂。
- **Wiring lives in `app`**: 新增组件时在 `app.New` 里构造并挂到 `App` 字段上，启动/关闭动作放 `app.Start`/`app.Shutdown`。不要在 `main`、`router`、`bot`、`web/server` 里 new 组件或读包级全局来绕过注入。
