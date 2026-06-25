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
onebot → config
cache → config
llm → (no internal deps)
config → (no internal deps)
logutil → (no internal deps)
```

`bot` is the orchestrator; `cmd` handles command routing and the handler registry; `onebot` is the NapCat HTTP client; `cache` holds per-group ring buffers; `llm` is the OpenAI-compatible client; `logutil` sets up the daily rotating logger.

## Startup sequence

1. `logutil.SetupLogger()` — daily rotating log to `exe_dir/log/`
2. `config/config.go` init() — loads `config.yaml` into package-level vars
3. `config/prompt.go` init() — loads `prompt_system.yaml` → `CmdConfigs` + `SharedRules`, merges `prompt_custom.yaml`
4. `cmd/internal_cmd.go` init() — `Register()` internal commands into the `registry`
5. `cmd/router.go` init() — `RebuildRoutes()` builds `Routes` from registry + CmdConfigs
6. `main.go` — instantiates `llm.DefaultClient`, fetches bot nickname via `onebot.GetLoginInfo()`, starts `bot.RunPollingLoop()`

## Config files

| File | Loaded by | Hot-reload |
|---|---|---|
| `config.yaml` | `config/config.go` init() | No |
| `prompt_system.yaml` | `config/prompt.go` init() | Yes (`ReloadPrompts()`) |
| `prompt_custom.yaml` | merged into `CmdConfigs` at startup | Yes (`ReloadPrompts()`) |

`config.yaml` has four sections: `napcat`, `bot`, `runtime`, `llm`. Prompt files have `cmd:` (map of category → list of `{keyword, prompt}`) and `rules:` (map of category → shared rules string appended to every prompt of that category).

All config values are exported as package-level vars (`config.BotQQ`, `config.LLMConfig`, `config.CmdConfigs`, `config.SharedRules`).

## Command system (`cmd/`)

### Two kinds of commands

| Kind | Defined in | Examples |
|---|---|---|
| Internal | `cmd/internal_cmd.go` via `Register()` | `添加关键字`, `删除关键字`, `帮助` |
| User | `prompt_system.yaml` / `prompt_custom.yaml` YAML lists | `锐评下`, `猫娘来看看` |

### Command struct and registry (`cmd/command.go`)

```go
type Command struct {
    Keyword     string
    Help        string
    Category    string  // "chat_review" | "direct_ask" | "internal"
    SharedRules string
    Handler     func(onebot.Event, string, string)
}
func Register(cmd Command)  // appends to registry (call in init())
func RebuildRoutes()        // rebuilds Routes from registry + CmdConfigs
func IsInternalKeyword(keyword string) bool
```

### Route building (`RebuildRoutes`)

1. System routes: iterate `registry`, create one `Route` per `Command`
2. User routes: iterate `config.CmdConfigs`, look up handler in `handlerMap`, create one `Route` per keyword

`handlerMap` maps YAML category names to handler functions:
```go
var handlerMap = map[string]func(onebot.Event, string, string){
    "chat_review": chatReview,
}
```

### Route dispatch (`cmd/router.go`)

`RouteMessage(content, event, groupID)`:
1. `stripCQPrefix()` — strips `[CQ:at,qq=xxx]` codes and `@Nickname` text
2. Linear scan of `Routes`, match by `strings.HasPrefix(text, keyword)`
3. Extra text after keyword becomes `"用户补充,优先级很高:{extra}"` appended to prompt
4. Prompt is wrapped with bot identity: QQ, nickname, and mentioner's nickname
5. Handler receives `(event, groupID, enrichedPrompt)`

### Message flow

```
polling (bot/polling.go) → fetch history (onebot)
                         → dedup via cache.HasMsgID
                         → ProcessMessage (bot/handler.go)
                            → whitelist check
                            → truncate to MaxMsgRune
                            → add to ring cache
                            → @bot detection (QQ number + nickname)
                            → cmd.RouteMessage → handler
```

## Adding a new command type

1. Write a handler: `func handlerName(event onebot.Event, groupID string, prompt string)`
2. Add to `handlerMap` in `cmd/command.go`: `"category_name": handlerName`
3. Add entries in `prompt_system.yaml` under `cmd.category_name:` as a list of `{keyword, prompt}`
4. Optionally add shared rules under `rules.category_name:`

Routes are auto-generated. No router changes needed.

## Internal commands

Defined purely in Go (no YAML). Each calls `Register(Command{...})` in `init()`. Currently three: add keyword (prompt via LLM), delete keyword, help listing.

`添加关键字` format: `添加关键字(关键词)指令(类型)大模型想提示词(要点)` — the LLM generates the prompt from the requirements. Writing goes to `prompt_custom.yaml`.

`删除关键字` format: `删除关键字(关键词)`. Both refuse to touch keywords that exist in `prompt_system.yaml` or in the registry.

## @mention detection (`bot/handler.go`)

`isAtBot(rawMsg)` checks two things: `strings.Contains(rawMsg, config.BotQQ)` (catches CQ codes like `[CQ:at,qq=xxx]`), and `strings.Contains(rawMsg, "@"+config.BotNickname)` (catches text @mentions). The nickname is fetched at startup via `onebot.GetLoginInfo()`; failure is non-fatal.

## LLM client (`llm/`)

```go
type Client interface {
    Review(ctx context.Context, chatLog, systemPrompt string) (string, error)
}
```

`OpenAIAdapter` implements `Client`. Sends `model`, `temperature`, `top_p`, and `messages` to `{apiBase}/chat/completions`. The user message is hardcoded as `"以下是群聊记录：\n{chatLog}\n请回复"`. Set via `llm.DefaultClient = llm.NewOpenAIAdapter(...)`.

## Ring buffer cache (`cache/`)

Per-group `GroupMsgCache` via `GetGroupCache(groupID)`. Fixed capacity (`config.MaxCacheMsg`), oldest evicted. `HasMsgID()` deduplicates. `BuildChatLog(msgs)` formats `"昵称：内容\n"` lines. Chat log is sent to LLM with all cached messages — no filtering or prioritization.

## Logging (`logutil/`)

`SetupLogger()` creates a `log/` directory next to the exe. One file per day (`2026-06-25.log`), sliced at 20MB (`2026-06-25_1.log`), cleaned after 30 days. Writes to both stdout and file via `io.MultiWriter`.

## Config notes

- `config.yaml` and `prompt_system.yaml` contain real credentials/prompts — NOT committed
- `prompt_custom.yaml` is created automatically, also NOT committed
- `config_example.yaml` is the committed template
- YAML with `#` comments, standard `gopkg.in/yaml.v3` parsing
- `resolveConfigPath(filename)` searches `./` then `exeDir/`
- `customPromptPath()` forces `prompt_custom.yaml` into the same directory as `prompt_system.yaml`
