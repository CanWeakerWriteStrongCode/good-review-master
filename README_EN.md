<p align="center">
  <a href="README.md">中文</a> | <a href="README_EN.md">English</a>
</p>

---

<h1 align="center">Not Good Review Master</h1>

<p align="center">A QQ group chatbot powered by NapCatQQ + LLM — @mention the bot to get AI-generated sharp reviews of group chat, ask questions, or dynamically add custom commands.</p>

## Features

- **@mention + keyword trigger**: @mention the bot with a keyword (e.g. "锐评下") to trigger LLM responses
- **Context-aware**: Uses recent group chat history as context for relevant, tailored responses — not random replies
- **Pluggable commands**: Add new keyword variants in `prompt_system.yaml` — no code changes needed
- **In-chat dynamic commands**: Add or delete custom keywords directly from the group chat via internal commands
- **Whitelist**: Only responds in configured group IDs
- **Local network only**: Go backend polls NapCatQQ's local HTTP API — no public IP required
- **Single binary deployment**: Compile to one executable, drop it on a server, and run

## Architecture

```
QQ ←→ NapCatQQ (local HTTP API) ←→ Go Bot (polling) ←→ LLM API (OpenAI-compatible)
```

```
┌──────────┐     HTTP      ┌────────────┐     HTTP      ┌──────────┐
│ QQ Group │ ←──────────→ │  NapCatQQ   │ ←──────────→ │  Go Bot  │
└──────────┘               └────────────┘               └─────┬────┘
                                                              │
                                                              │ OpenAI API
                                                              ▼
                                                      ┌──────────────┐
                                                      │  LLM Model   │
                                                      │ (DeepSeek…)  │
                                                      └──────────────┘
```

### Message Flow

```
Polling loop (bot/polling.go)
  → Fetch history from NapCat (onebot, resty)
  → Dedup via O(1) msgID set
  → ProcessMessage (bot/handler.go)
     → Whitelist check
     → Truncate to max length
     → Store in ring buffer (zero-copy write)
     → @bot detection (QQ number + nickname)
     → Prefix trie match → command handler
```

## Quick Start

### Prerequisites

- [NapCatQQ](https://github.com/NapNeko/NapCatQQ) installed and logged in (HTTP service enabled)
- An OpenAI-compatible LLM API key (DeepSeek, Doubao, Tongyi Qianwen, etc.)

### Run from source

> Install [Go](https://go.dev/dl/) 1.25+ and [Node.js](https://nodejs.org/) 18+ first.

```bash
# Windows: double-click start_main.bat
# Linux/macOS: ./start_main.sh
# The script runs 3 steps automatically:
#   1. Install frontend deps + build frontend
#   2. Copy to embed directory
#   3. go run main.go
# First run auto-creates config.yaml.
# Edit config.yaml with your settings, then run again.
```

### Run compiled exe

Drop the exe in an empty directory and run it. On first launch, if `config.yaml` is missing, the exe auto-creates it from a built-in template. Edit the generated `config.yaml` and restart.

### Build executable

> Building also requires [Git](https://git-scm.com/) for automatic version extraction.

```bash
# Windows: double-click build_exe.bat
# Linux/macOS: ./build_linux.sh
# The script runs 3 steps automatically:
#   1. Install frontend deps + build frontend
#   2. Copy to embed directory
#   3. Cross-compile 4 platform binaries into dist/
#
# Version is auto-extracted from git tags — no manual versioning:
#   Tagged v1.0.0  → filename ends with v1.0.0
#   No tag         → filename ends with abc1234 (commit hash)
#   Uncommitted changes → filename ends with abc1234-dirty
#
# Output:
#   dist/good-review-master-windows-amd64-v1.0.0.exe
#   dist/good-review-master-linux-amd64-v1.0.0
#   dist/good-review-master-darwin-amd64-v1.0.0     (Intel Mac)
#   dist/good-review-master-darwin-arm64-v1.0.0     (Apple Silicon)
# Copy the matching binary to any directory and run it.
# First run auto-creates config.yaml.
# Edit config.yaml with your settings, then run again.
```

## Configuration

### config.yaml

| Key | Description | Example |
|-----|-------------|---------|
| `napcat.http_api` | NapCatQQ HTTP API URL | `http://127.0.0.1:3000` |
| `napcat.access_token` | NapCatQQ access token (set in WebUI) | `""` |
| `bot.qq` | Bot's QQ number | `123456` |
| `bot.allow_groups` | Allowed group IDs (comma-separated) | `123456,789012` |
| `llm.provider` | Always `openai` (OpenAI-compatible) | `openai` |
| `llm.api_key` | LLM API key | `sk-xxx` |
| `llm.api_base` | LLM API base URL | `https://api.deepseek.com` |
| `llm.model_name` | Model name | `deepseek-v4-flash` |
| `llm.temperature` | Sampling temperature (1.0=creative, 0.5=focused) | `1.2` |
| `llm.top_p` | Nucleus sampling (lower = more focused) | `0.95` |
| `llm.cache_hit_cost` | Cache hit cost (relative) | `0.033` |
| `llm.cache_miss_cost` | Cache miss cost (relative) | `1.0` |
| `llm.max_context_tokens` | Max context tokens per LLM request (force reset if exceeded) | `50000` |
| `runtime.llm_send_count` | Recent messages sent per request (reset window) | `200` |
| `runtime.max_cache_msg` | Max ring-buffer messages (≈31×llm_send_count) | `6200` |
| `runtime.llm_timeout_sec` | LLM timeout (seconds) | `20` |
| `runtime.max_msg_rune` | Max characters per message | `500` |
| `runtime.poll_interval_sec` | Poll interval (seconds) | `3` |
| `runtime.web_port` | Web panel port (<=0 to disable) | `8080` |
| `runtime.web_username` | Web panel login username | `admin` |
| `runtime.web_password` | Web panel login password (required) | `"123456"` |

### Web Management Panel

Gin + JWT + embedded Vue SPA for group message monitoring.

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/api/login` | No | Returns JWT token (validates username/password) |
| GET | `/api/status` | JWT | BotQQ, Nickname, Masked API Key, group count |
| GET | `/api/groups` | JWT | Per-group info with activity stats |
| GET | `/api/groups/:id` | JWT | Cached messages for one group |
| POST | `/api/logout` | JWT | No-op (stateless token) |

If password is empty, the login page is skipped entirely. Set `web_port` to 0 or negative to disable the web panel.

### prompt_system.yaml

Commands are defined in list format. One command type can have multiple keyword + prompt variants:

```yaml
cmd:
  chat_review:            # Sends recent chat log to LLM
    - keyword: "锐评下"
      prompt: |
        You are a sharp-tongued group chat review bot.
        Based on the chat records, make a witty summary.
    - keyword: "猫娘来看看"
      prompt: |
        You are a cute catgirl. Pick the cutest group member and compliment them.

rules:
  chat_review: |          # Shared rules appended to every chat_review prompt
    1. No personal attacks or prohibited content
    2. Keep under 100 characters
    3. Output the result directly, no extra explanation
    4. Pay more attention to the most recent 10 messages
```

Add a new variant by adding an entry under the same list — no code changes needed.

## Command System

### Two kinds of commands

| Kind | Defined in | Examples |
|---|---|---|
| **Function commands** | `prompt_system.yaml` / `prompt_custom.yaml` | `锐评下`, `猫娘来看看` |
| **Internal commands** | Go code (`cmd/internal_cmd.go`) | `添加关键字`, `删除关键字`, `帮助` |

### Triggering

@mention the bot followed by a keyword. Extra text after the keyword is passed to the LLM as priority context:

```
@bot 猫娘来看看 what do you think of the recent messages?
```

### Dynamic commands (from group chat)

Add a new keyword directly from the group — the LLM generates the prompt:

```
@bot 添加关键字(meanie-review)指令(chat_review)大模型想提示词(foul-mouthed, roasts everyone, calls them old)
```

Delete a keyword:

```
@bot 删除关键字(meanie-review)
```

System keywords (from `prompt_system.yaml`) and internal commands cannot be overwritten or deleted. Dynamically added keywords are saved to `prompt_custom.yaml` and persist across restarts.

### Get help

```
@bot 帮助
```

Lists all available commands with usage instructions and available command types.

## Extending Commands

### Add a variant (no code)

Add a new entry under the desired category in `prompt_system.yaml`:

```yaml
cmd:
  chat_review:
    - keyword: "雌小鬼锐评下"
      prompt: |
        You are a foul-mouthed little brat who roasts everyone.
```

### Add a new command type (requires code)

Three steps:

**1. Add new type config under `cmd:` in `prompt_system.yaml`**

```yaml
cmd:
  weather:
    - keyword: "weather"
      prompt: "You are a weather assistant..."
```

**2. Create a new handler file in `cmd/`** (e.g. `weather.go`), handler must be a Router method:

```go
func (r *Router) weatherHandler(event onebot.Event, groupID string, prompt string) {
    r.Go(func(ctx context.Context) error {
        // Safe async: ctx auto-inherits shutdown signal
        reply, err := r.llmClient.Review(ctx, chatLog, prompt)
        ...
        return nil
    })
}
```

**3. Register in `handlerMap` in `cmd/command.go` `NewRouter()`**

```go
r.handlerMap = map[string]HandlerFunc{
    "chat_review": r.chatReview,
    "weather":     r.weatherHandler,  // add this line
}
```

## Project Structure

```
good-review-master/
├── main.go                  # Entry point: init config, LLM client, start polling + graceful shutdown
├── go.mod / go.sum           # Go module dependencies
├── config.yaml               # Live config (gitignored)
├── prompt_system.yaml        # System prompts (gitignored)
├── prompt_custom.yaml        # Dynamic prompts (gitignored, auto-created)
├── start_main.bat / .sh      # Dev launcher scripts (includes frontend build)
├── build_exe.bat / .sh       # Build & package scripts (cross-compile 4 targets)
├── version/
│   └── version.go            # Version injection (via -ldflags at build time)
├── apppath/
│   └── apppath.go            # Resolve paths relative to executable
├── pool/
│   └── pool.go               # Generic goroutine pool (fixed workers, bounded queue)
├── config/
│   ├── config.go             # Runtime config (config.yaml → struct)
│   ├── config_example.yaml   # Built-in config template (embedded at build time)
│   ├── prompt_system_example.yaml  # Built-in prompt template (embedded at build time)
│   ├── embed.go              # //go:embed template embedding
│   ├── init.go               # Auto-create config files on first run
│   └── prompt.go             # Prompt config loading + hot-reload + CRUD
├── cache/
│   └── cache.go              # Per-group ring buffer (zero-copy writes, O(1) dedup)
├── llm/
│   └── llm.go                # LLM client (go-openai SDK, connection pooling, typed)
├── async/
│   └── async.go              # Safe goroutine manager (pool-based + auto ctx + panic recover)
├── logutil/
│   └── logger.go             # Logging (zap + lumberjack, 20MB rotation, 30-day retention)
├── onebot/
│   ├── client.go             # NapCatQQ HTTP API client (resty, auto-marshal + retry)
│   └── types.go              # API data types
├── bot/
│   ├── polling.go            # HTTP poll loop + history fetching (context-aware)
│   └── handler.go            # Message processing: whitelist → @detection → routing
├── cmd/
│   ├── command.go            # Router + prefix trie matching + safe goroutine launch
│   ├── internal_cmd.go       # Internal commands (add/delete keyword, help)
│   └── chat_review.go        # Async chat_review handler
└── web/
    ├── server/                # Gin web management panel backend
    │   ├── server.go          # Gin engine, API routes, SPA fallback, graceful shutdown
    │   ├── handlers.go        # API handlers (login/status/groups/messages)
    │   ├── auth.go            # JWT signing & validation (HS256, 24h expiry)
    │   ├── middleware.go       # Middleware (logging, panic recovery, CORS, JWT auth)
    │   └── embed.go           # //go:embed static/frontend
    └── frontend/              # uni-app Vue 3 SPA
        └── src/               # Pages: login, groups list, message detail
```

### Package Dependency Graph

```
main → config, llm, logutil, bot, onebot, async, apppath, version, web/server
bot → config, cache, onebot, cmd
cmd → config, cache, llm, onebot, async
web/server → config, logutil, onebot, cache
async → logutil, pool
pool → (no internal deps: stdlib sync only)
onebot → (no internal deps)
cache → (no internal deps)
llm → (no internal deps)
config → apppath, logutil
logutil → apppath
apppath → (no internal deps)
version → (no internal deps)
```

## Logging

Logs are written to the `log/` directory under the working directory. Uses `zap` + `lumberjack`: size-based rotation at 20MB, max 30 backup files, 30-day retention, gzip-compressed old files. Logs go to both stdout (colored) and file.

## Deployment

- Local machine or cloud server — no public IP needed
- Compile to a single binary (frontend SPA embedded), no runtime dependencies
- **Auto-create config on first launch**: If `config.yaml` and `prompt_system.yaml` are missing, the exe auto-generates them from built-in templates. Edit and restart.
- Use `systemd` (Linux) or Task Scheduler (Windows) for auto-start on boot
