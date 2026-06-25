# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Verify

```bash
go vet ./...          # check all packages
go build ./...        # verify all packages compile
go build -o good-review-master.exe .  # build binary
```

No tests. Dependencies: `gopkg.in/yaml.v3` (config parsing).

## Architecture

```
QQ ←→ NapCatQQ (local HTTP API) ←→ Go bot (polling) ←→ LLM API (OpenAI-compatible)
```

### Package graph

```
main → config, llm, logutil, bot, onebot
bot → config, cache, onebot, cmd
cmd → config, cache, llm, onebot
onebot → (no internal deps)
cache → (no internal deps)
llm → (no internal deps)
config → apppath
logutil → apppath
apppath → (no internal deps)
```

`bot` is the orchestrator; `cmd` handles command routing and the handler registry; `onebot` is the NapCat HTTP client; `cache` holds per-group ring buffers; `llm` is the OpenAI-compatible client; `logutil` sets up the daily rotating logger; `apppath` resolves config file paths relative to the executable.

### Key design: explicit dependency injection, no init() side effects

All components are constructed explicitly in `main()`. There are **zero** `init()` functions with cross-package side effects — no more init-time ordering dependencies. Dependencies flow top-down through struct fields and constructor parameters.

## Startup sequence

1. `logutil.SetupLogger()` — daily rotating log to `exe_dir/log/`
2. `config.LoadConfig(path)` — loads `config.yaml` → `*Config` struct
3. `config.LoadPromptConfig(systemPath, customPath)` — loads `prompt_system.yaml` + `prompt_custom.yaml` → `*PromptConfig` struct
4. `llm.NewOpenAIAdapter(...)` — creates `llm.Client` from config
5. `onebot.NewClient(httpAPI, accessToken)` — creates OneBot HTTP client
6. `cmd.NewRouter(cfg, promptCfg, llmClient, obClient)` — creates router, registers internal commands, builds initial route table
7. `obClient.GetLoginInfo()` — fetches bot nickname, stores in `cfg.BotNickname`
8. `bot.NewBot(cfg, obClient, router)` + `botInstance.RunPollingLoop()` — starts polling

## Config files

| File | Loaded by | Hot-reload |
|---|---|---|
| `config.yaml` | `config.LoadConfig()` | No |
| `prompt_system.yaml` | `config.LoadPromptConfig()` | Yes (`PromptConfig.Reload()`) |
| `prompt_custom.yaml` | merged into `PromptConfig` at startup | Yes (`PromptConfig.Reload()`) |

`config.yaml` has four sections: `napcat`, `bot`, `runtime`, `llm`. Prompt files have `cmd:` (map of category → list of `{keyword, prompt}`) and `rules:` (map of category → shared rules string appended to every prompt of that category).

### Config struct (`config/config.go`)

All runtime settings are fields on the `Config` struct (no package-level vars):
```go
type Config struct {
    NapCatHTTPAPI, NapCatAccessToken, BotQQ, BotNickname string
    AllowGroups       []string   // parsed from comma-separated
    MaxCacheMsg       int
    LLMTimeout        time.Duration
    MaxMsgRune        int
    PollInterval      time.Duration
    LLMConfig         LLMConf
}
```

### PromptConfig struct (`config/prompt.go`)

```go
type PromptConfig struct {
    CmdConfigs  map[string][]CmdConf
    SharedRules map[string]string
    // methods: Reload, AddCommand, DeleteCommand, AddRule, DeleteRule,
    //          KeywordInMainPrompt, KeywordInMainPromptAny, RuleInMainPrompt
}
```

## Command system (`cmd/`)

### Two kinds of commands

| Kind | Defined in | Examples |
|---|---|---|
| Internal | `cmd/internal_cmd.go` via `Router.register()` | `添加关键字`, `删除关键字`, `帮助` |
| User | `prompt_system.yaml` / `prompt_custom.yaml` YAML lists | `锐评下`, `猫娘来看看` |

### Router struct (`cmd/command.go`)

All routing state is encapsulated in the `Router` struct:
```go
type Router struct {
    // unexported fields: routes, registry, handlerMap, llmClient, obClient, promptCfg, appCfg
}
func NewRouter(appCfg, promptCfg, llmClient, obClient) *Router  // constructor, auto-registers internal commands
func (r *Router) RouteMessage(content, event, groupID)          // match and dispatch
```

`cmd/router.go` has been merged into `cmd/command.go`. There is no more `init()` — internal commands are registered in `NewRouter` → `registerInternalCommands()`.

### Route building (`Router.rebuild()`)

1. System routes: iterate `registry`, create one `Route` per `Command`
2. User routes: iterate `promptCfg.CmdConfigs`, look up handler in `handlerMap`, create one `Route` per keyword

`handlerMap` maps YAML category names to handler methods:
```go
r.handlerMap = map[string]HandlerFunc{
    "chat_review": r.chatReview,
}
```

### Route dispatch (`Router.RouteMessage()`)

1. `stripCQPrefix()` — strips `[CQ:at,qq=xxx]` codes and `@Nickname` text
2. Linear scan of routes, match by `strings.HasPrefix(text, keyword)`
3. Extra text after keyword becomes `"用户补充,优先级很高:{extra}"` appended to prompt
4. Prompt is wrapped with bot identity: QQ, nickname, and mentioner's nickname
5. Handler receives `(event, groupID, enrichedPrompt)`

### Message flow

```
polling (bot/polling.go) → fetch history (onebot.Client)
                         → dedup via cache.HasMsgID
                         → ProcessMessage (bot/handler.go)
                            → whitelist check (Config.HasGroup)
                            → truncate to MaxMsgRune
                            → add to ring cache
                            → @bot detection (QQ number + nickname)
                            → router.RouteMessage → handler
```

## Adding a new command type

1. Write a handler method: `func (r *Router) handlerName(event onebot.Event, groupID string, prompt string)`
2. Add to `handlerMap` in `cmd/command.go` `NewRouter()`: `"category_name": r.handlerName`
3. Add entries in `prompt_system.yaml` under `cmd.category_name:` as a list of `{keyword, prompt}`
4. Optionally add shared rules under `rules.category_name:`

Routes are auto-generated. No router changes needed.

## Internal commands

Defined purely in Go (no YAML). Registered in `registerInternalCommands()` called from `NewRouter()`. Currently five: add keyword (prompt via LLM), delete keyword, add rule, delete rule, help listing.

`添加关键字` format: `添加关键字(关键词)指令(指令类型)大模型想提示词(要点)` — the LLM generates the prompt from the requirements. Writing goes to `prompt_custom.yaml`.

`删除关键字` format: `删除关键字(关键词)`. Both refuse to touch keywords that exist in `prompt_system.yaml` or in the registry.

## @mention detection (`bot/handler.go`)

`Bot.isAtBot(rawMsg)` checks two things: `strings.Contains(rawMsg, b.cfg.BotQQ)` (catches CQ codes like `[CQ:at,qq=xxx]`), and `strings.Contains(rawMsg, "@"+b.cfg.BotNickname)` (catches text @mentions). The nickname is fetched at startup via `onebot.Client.GetLoginInfo()`; failure is non-fatal.

## LLM client (`llm/`)

```go
type Client interface {
    Review(ctx context.Context, chatLog, systemPrompt string) (string, error)
}
```

`OpenAIAdapter` implements `Client`. Sends `model`, `temperature`, `top_p`, and `messages` to `{apiBase}/chat/completions`. The user message is hardcoded as `"以下是群聊记录：\n{chatLog}\n请回复"`. Created via `llm.NewOpenAIAdapter(...)` in `main()`, injected into `cmd.Router` and used by handlers. No global `DefaultClient`.

## OneBot client (`onebot/`)

```go
type Client struct { /* unexported: httpAPI, accessToken, httpClient */ }
func NewClient(httpAPI, accessToken string) *Client
func (ob *Client) GetLoginInfo() (*LoginInfo, error)
func (ob *Client) SendGroupMessage(groupID, content string)
func (ob *Client) FetchGroupMsgHistory(groupID string, count int) ([]HistoryMsg, error)
```

No dependency on `config` package — endpoint and token are passed via `NewClient()`.

## Ring buffer cache (`cache/`)

Per-group `GroupMsgCache` via `GetGroupCache(groupID, maxSize)`. Fixed capacity (`maxSize` field), oldest evicted. `HasMsgID()` deduplicates. `BuildChatLog(msgs)` formats `"昵称：内容\n"` lines. Chat log is sent to LLM with all cached messages — no filtering or prioritization.

## Logging (`logutil/`)

`SetupLogger()` creates a `log/` directory next to the exe. One file per day (`2026-06-25.log`), sliced at 20MB (`2026-06-25_1.log`), cleaned after 30 days. Writes to both stdout and file via `io.MultiWriter`.

## Config notes

- `config.yaml` and `prompt_system.yaml` contain real credentials/prompts — NOT committed
- `prompt_custom.yaml` is created automatically, also NOT committed
- `config_example.yaml` is the committed template
- YAML with `#` comments, standard `gopkg.in/yaml.v3` parsing
- `apppath.ResolvePath(filename)` searches `./` then `exeDir/`
- `config.CustomPromptPath(systemPath)` gives `prompt_custom.yaml` path in the same directory as `prompt_system.yaml`

## Code conventions

- **Variable naming**: 变量名尽量是要有场景区分度的多单词组合，禁止单字母或缩写。接收者命名用类型名的有意义简写。
