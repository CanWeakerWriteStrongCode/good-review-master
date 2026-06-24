# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build & Verify

```bash
go vet ./...          # check all packages
go build ./...        # verify all packages compile
go build -o good-review-master.exe .  # build binary
```

No tests currently exist. Dependencies: `gopkg.in/yaml.v3` (config parsing), `gorilla/websocket` (declared in go.mod but unused — reserved for future WebSocket mode).

## Architecture

QQ ←→ NapCatQQ (local HTTP API) ←→ Go bot (polling) ←→ LLM API (OpenAI-compatible)

### Package dependency graph

```
main → config, llm, bot
bot → config, cache, onebot, cmd
cmd → config, cache, llm, onebot
onebot → config
cache → config
llm → (no internal deps)
config → (no internal deps)
```

No circular dependencies. `bot` is the top-level orchestrator; `cmd` handles command routing; `onebot` is the NapCat HTTP client; `cache` holds the message ring buffer; `llm` is the LLM adapter.

### Config loading (`config/`)

`config.yaml` is read at startup via `init()` in `config/config.go`. Prompt are loaded separately from `prompt_system.yaml` via `init()` in `config/prompt.go`.

All config values are exported as package-level vars (e.g., `config.BotQQ`, `config.LLMConfig`, `config.CmdConfigs`).

### Command routing (`cmd/`)

`cmd/router.go` defines `Routes`, a slice of `Route{Keyword, Prompt, Handler}`. `bot/handler.go` calls `cmd.RouteMessage()` which iterates routes and dispatches to the matching handler.

Handlers have the signature `func(event onebot.Event, groupID string, prompt string)`. The prompt is passed from the route config to the handler — handlers don't read config directly.

### LLM client (`llm/`)

```go
type Client interface {
    Review(ctx context.Context, chatLog, systemPrompt string) (string, error)
}
```

The prompt is NOT baked into the adapter — each handler passes its own prompt. `llm.DefaultClient` is the global instance, set by `main()`.

### Message flow

```
polling (bot/polling.go) → fetch history (onebot)
                         → dedup via cache.HasMsgID
                         → ProcessMessage (bot/handler.go)
                            → whitelist check
                            → emoji filter
                            → add to ring cache
                            → @bot detection
                            → cmd.RouteMessage → handler
```

### Ring buffer cache (`cache/`)

Per-group `GroupMsgCache`, initialized lazily via `GetGroupCache(groupID)`. Fixed capacity (`config.MaxCacheMsg`), oldest evicted when full. `HasMsgID()` deduplicates across polls.

## Adding a new command

Three steps, no changes to `config/config.go` needed:

1. Add config in `prompt_system.yaml` under `cmd:`:
   ```yaml
   weather:
     keyword: "天气"
     prompt: "你是天气助手..."
   ```

2. Create handler in `cmd/weather.go`:
   ```go
   func weather(event onebot.Event, groupID string, prompt string) {
       // call llm.DefaultClient.Review(ctx, chatLog, prompt) or send static reply
   }
   ```

3. Add route in `cmd/router.go`:
   ```go
   {Keyword: config.CmdConfigs["weather"].Keyword, Prompt: config.CmdConfigs["weather"].Prompt, Handler: weather},
   ```

## Configuration notes

- `config.yaml` contains real credentials — NOT committed to git (the user manages this manually)
- `config_example.yaml` is the template for new users
- `prompt_system.yaml` contains LLM prompt — also NOT committed
- YAML with `#` comments, standard Go yaml.v3 parsing
