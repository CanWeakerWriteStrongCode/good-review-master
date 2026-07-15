# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Verify

```bash
go vet ./...          # check all packages
go build ./...        # verify all packages compile
go build -o good-review-master.exe .  # build binary
```

No tests.

## Build scripts

The frontend **must** be compiled before the Go binary — Go's `embed` resolves files at compile time. All build scripts follow this 3-step process:

| Script | Step 3 | Platform |
| --- | --- | --- |
| `build_exe.bat` | `go build -o good-review-master.exe .` | Windows |
| `build_linux.sh` | `go build -o good-review-master .` | Linux |
| `start_main.bat` | `go run main.go` | Windows |
| `start_main.sh` | `go run main.go` | Linux |

1. `pnpm run build:h5` (in `web/frontend/`) — builds uni-app H5 frontend
2. Copy `dist/build/h5` → `web/server/static/frontend/`
3. `go build` or `go run`

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
main → config, llm, logutil, bot, onebot, async
main → web/server
bot → config, cache, onebot, cmd
cmd → config, cache, llm, onebot, async
web/server → config, logutil, onebot, cache
async → logutil, pool
pool → (仅标准库 sync)
onebot → (no internal deps)
cache → (no internal deps)
llm → (no internal deps)
config → apppath
logutil → apppath
apppath → (no internal deps)
```

`bot` is the orchestrator; `cmd` handles command routing with a prefix trie; `web/server` provides the web management panel (Gin + SPA); `async` provides safe goroutine launching with automatic context propagation; `onebot` is the NapCat HTTP client (resty-based); `cache` holds per-group zero-copy ring buffers; `llm` is the OpenAI-compatible client (go-openai SDK); `logutil` wraps zap + lumberjack; `apppath` resolves config file paths relative to the executable.

### Key design: explicit dependency injection, no init() side effects

All components are constructed explicitly in `main()`. There are **zero** `init()` functions with cross-package side effects. Dependencies flow top-down through struct fields and constructor parameters.

## Startup & shutdown sequence

1. `logutil.SetupLogger()` — console + file logging to `log/bot.log`
2. `config.LoadConfig(path)` — loads `config.yaml` → `*Config` struct
3. `config.LoadPromptConfig(systemPath, customPath)` — loads + merges prompt YAML files
4. `llm.NewOpenAIAdapter(...)` — creates `llm.Client` (go-openai SDK)
5. `onebot.NewClient(httpAPI, accessToken)` — creates OneBot HTTP client (resty)
6. `signal.NotifyContext` → `cmd.NewRouter(cfg, promptCfg, llmClient, obClient, shutdownCtx)` — router receives shutdown context for goroutine lifecycle
7. `obClient.GetLoginInfo()` — fetches bot nickname
8. `go botInstance.RunPollingLoop(shutdownCtx)` — starts polling in background
9. `webserver.New(cfg, obClient)` + `go webSrv.Start()` — starts web server (conditional, `cfg.WebPort > 0`)
10. `<-shutdownCtx.Done()` — blocks until SIGINT/SIGTERM
11. `webSrv.Shutdown(ctx)` — graceful web shutdown with 10s timeout (conditional)
12. `router.Wait()` — waits for in-flight goroutines to finish

## Config files

| File | Loaded by | Hot-reload |
| --- | --- | --- |
| `config.yaml` | `config.LoadConfig()` | No |
| `prompt_system.yaml` | `config.LoadPromptConfig()` | Yes (`PromptConfig.Reload()`) |
| `prompt_custom.yaml` | merged into `PromptConfig` at startup | Yes (`PromptConfig.Reload()`) |

All three YAML files are auto-created from embedded templates on first run if missing. `config.yaml` and `prompt_system.yaml` use templates under `config/`; `prompt_custom.yaml` is created empty on first keyword addition.

`config.yaml` has four sections: `napcat`, `bot`, `runtime`, `llm`. Prompt files have `cmd:` (map of category → list of `{keyword, prompt}`) and `rules:` (map of category → shared rules string appended to every prompt of that category).

`prompt_system.yaml` is parsed once at startup and cached (`Config.systemPrompt`) — subsequent checks read the cached pointer, not the file. `prompt_custom.yaml` is read on every write operation (add/delete command/rule) and on `Reload()`.

## Command system (`cmd/`)

### Two kinds of commands

| Kind | Defined in | Examples |
| --- | --- | --- |
| Internal | `cmd/internal_cmd.go` via `Router.register()` | `添加关键字`, `删除关键字`, `帮助` |
| User | `prompt_system.yaml` / `prompt_custom.yaml` YAML lists | `锐评下`, `猫娘来看看` |

### Router struct (`cmd/command.go`)

```go
type Router struct {
    routeTrie  *trieNode         // 前缀树匹配，O(k)
    routes     []Route           // 帮助列表遍历
    registry   []Command
    handlerMap map[string]HandlerFunc
    llmClient  llm.Client
    obClient   *onebot.Client
    promptCfg  *config.PromptConfig
    appCfg     *config.Config
    starter    *async.Group     // goroutine 生命周期管理
}
func NewRouter(appCfg, promptCfg, llmClient, obClient, shutdownCtx) *Router
func (r *Router) RouteMessage(content, event, groupID)
func (r *Router) Go(fn func(context.Context) error)   // 安全启动 goroutine
func (r *Router) Wait() error                         // 等待所有 goroutine 退出
```

### Route matching: prefix trie

Routes are stored in a prefix trie (`trieNode`), NOT a flat slice. Matching walks the trie character by character and returns the **longest matching prefix** — e.g., "锐评下" matches before "锐评".

```go
func trieMatch(root *trieNode, text string) *Route   // O(k), k = len(text)
```

`rebuild()` iterates all routes and inserts them into the trie. A flat `[]Route` is also maintained for the `帮助` command to list all user commands.

### Route dispatch (`Router.RouteMessage()`)

1. `stripCQPrefix()` — strips `[CQ:at,qq=xxx]` codes and `@Nickname` text
2. `trieMatch()` — longest prefix match on cleaned text
3. Extra text after keyword becomes `"用户补充,优先级很高:{extra}"` appended to prompt
4. Prompt is wrapped with bot identity: QQ, nickname, and mentioner's nickname
5. Handler receives `(event, groupID, enrichedPrompt)`

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
2. Add to `handlerMap` in `cmd/command.go` `NewRouter()`: `"category_name": r.handlerName`
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
| `auth.go` | JWT generation (HS256, 24h expiry) and parsing |
| `middleware.go` | Logger, Recovery (panic guard), CORS, JWT auth guard |
| `embed.go` | `//go:embed static/frontend` |

**API endpoints:**

| Method | Path | Auth | Description |
| --- | --- | --- | --- |
| POST | `/api/login` | No | Returns JWT token (or `need_password: false` if password empty) |
| GET | `/api/status` | JWT | BotQQ, Nickname, MaskedAPIKey, GroupCount |
| GET | `/api/groups` | JWT | Per-group info with activity stats |
| GET | `/api/groups/:id` | JWT | Cached messages for one group |
| POST | `/api/logout` | JWT | No-op (stateless token) |

Key details: Gin runs in ReleaseMode; CORS allows all origins; empty `web_password` bypasses auth entirely; `groupNames` map caches GetGroupInfo results to avoid repeated NapCat calls.

### Frontend (`web/frontend/`)

uni-app Vue 3 project — targets H5 web and WeChat MiniProgram. 3 pages: Login, Groups List, Message Detail. Pinia stores with token in `localStorage["good_review_token"]`. Hash routing for SPA compatibility.

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

## Logging (`logutil/`)

Uses `zap` + `lumberjack`. Dual output: console (colored) + file. `lumberjack` handles rotation: 20MB max, 30 backups, 30-day retention, gzip compression. `zap.AddCallerSkip(1)` makes caller field point to actual call site, not the `logutil` wrapper.

Thin wrappers: `Info(msg, kv...)`, `Error(msg, kv...)`, `Warn(msg, kv...)`, `Debug(msg, kv...)` — all delegate to `sugar.Infow/Errorw/Warnw/Debugw`.

## Config notes

- `config.yaml` and `prompt_custom.yaml` contain real credentials/user data — NOT committed
- `prompt_system.yaml` also NOT committed (auto-created from embedded `config/prompt_system_example.yaml` on first run, like config.yaml)
- `config_example.yaml` is the committed template, embedded via `//go:embed` and auto-copied to `config.yaml` on first run
- On first run, `config.yaml` is created from the embedded template and the program exits — edit it and re-run
- `prompt_system.yaml` is also auto-created with a comment header if missing
- `runtime.web_port` — web panel port, <=0 disables the web server
- `runtime.web_username` / `runtime.web_password` — login credentials (empty password = no auth required)
- `Config.MaskedAPIKey()` — returns API key with only first 4 and last 4 chars visible (e.g., `sk-9a****d8`), used by web API
- `apppath.ResolvePath(filename)` searches `./` then `exeDir/`
- `config.CustomPromptPath(systemPath)` gives `prompt_custom.yaml` path in the same directory as `prompt_system.yaml`

## Code conventions

- **Variable naming**: 变量名尽量是要有场景区分度的多单词组合，禁止单字母或缩写。接收者命名用类型名的有意义简写。
