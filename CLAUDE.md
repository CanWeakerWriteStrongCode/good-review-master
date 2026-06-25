# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Verify

```bash
go vet ./...          # check all packages
go build ./...        # verify all packages compile
go build -o good-review-master.exe .  # build binary
```

No tests.

## Dependencies

| Library | Purpose |
| --- | --- |
| `github.com/sashabaranov/go-openai` | OpenAI-compatible LLM client (typed structs, connection pooling, error propagation) |
| `github.com/go-resty/resty/v2` | HTTP client for NapCat API (auto-marshal, retry, auth auto-attach) |
| `go.uber.org/zap` | Structured logging |
| `gopkg.in/natefinch/lumberjack.v2` | Log rotation (size-based, 30-day retention, gzip compression) |
| `gopkg.in/yaml.v3` | Config YAML parsing |
| `golang.org/x/sync` | `errgroup` for goroutine lifecycle management |

## Architecture

```
QQ ←→ NapCatQQ (local HTTP API) ←→ Go bot (polling) ←→ LLM API (OpenAI-compatible)
```

### Package graph

```
main → config, llm, logutil, bot, onebot, safego
bot → config, cache, onebot, cmd
cmd → config, cache, llm, onebot, safego
safego → logutil (wraps errgroup + panic recover)
onebot → (no internal deps)
cache → (no internal deps)
llm → (no internal deps)
config → apppath
logutil → apppath
apppath → (no internal deps)
```

`bot` is the orchestrator; `cmd` handles command routing with a prefix trie; `safego` provides safe goroutine launching with automatic context propagation; `onebot` is the NapCat HTTP client (resty-based); `cache` holds per-group zero-copy ring buffers; `llm` is the OpenAI-compatible client (go-openai SDK); `logutil` wraps zap + lumberjack; `apppath` resolves config file paths relative to the executable.

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
9. `<-shutdownCtx.Done()` — blocks until SIGINT/SIGTERM
10. `router.Wait()` — waits for in-flight goroutines to finish

## Config files

| File | Loaded by | Hot-reload |
| --- | --- | --- |
| `config.yaml` | `config.LoadConfig()` | No |
| `prompt_system.yaml` | `config.LoadPromptConfig()` | Yes (`PromptConfig.Reload()`) |
| `prompt_custom.yaml` | merged into `PromptConfig` at startup | Yes (`PromptConfig.Reload()`) |

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
    starter    *safego.Group     // goroutine 生命周期管理
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
func (ob *Client) SendGroupMessage(groupID, content string)
func (ob *Client) FetchGroupMsgHistory(groupID string, count int) ([]HistoryMsg, error)
```

Uses `resty` — Base URL, auth token, and Content-Type set once in `NewClient()`. All methods use `SetBody()` + `SetResult()` for automatic JSON marshal/unmarshal. Built-in retry (2 attempts). No repeated boilerplate per endpoint. No dependency on `config` package.

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

## Safe goroutine management (`safego/`)

```go
type Group struct { /* wraps errgroup.Group + context */ }
func New(ctx context.Context) *Group
func (g *Group) Go(fn func(context.Context) error)  // auto ctx + panic recover
func (g *Group) Wait() error
```

Wraps `golang.org/x/sync/errgroup` with automatic context propagation and panic recovery. `Router` holds a `*safego.Group` and exposes `Go(fn)` / `Wait()` proxy methods. Handlers fire async work with `r.Go(func(ctx) ...)` — ctx is automatically derived from the shutdown context, so Ctrl+C cancels in-flight LLM calls.

## Logging (`logutil/`)

Uses `zap` + `lumberjack`. Dual output: console (colored) + file. `lumberjack` handles rotation: 20MB max, 30 backups, 30-day retention, gzip compression. `zap.AddCallerSkip(1)` makes caller field point to actual call site, not the `logutil` wrapper.

Thin wrappers: `Info(msg, kv...)`, `Error(msg, kv...)`, `Warn(msg, kv...)`, `Debug(msg, kv...)` — all delegate to `sugar.Infow/Errorw/Warnw/Debugw`.

## Config notes

- `config.yaml` and `prompt_system.yaml` contain real credentials/prompts — NOT committed
- `prompt_custom.yaml` is created automatically, also NOT committed
- `config_example.yaml` is the committed template
- `apppath.ResolvePath(filename)` searches `./` then `exeDir/`
- `config.CustomPromptPath(systemPath)` gives `prompt_custom.yaml` path in the same directory as `prompt_system.yaml`

## Code conventions

- **Variable naming**: 变量名尽量是要有场景区分度的多单词组合，禁止单字母或缩写。接收者命名用类型名的有意义简写。
