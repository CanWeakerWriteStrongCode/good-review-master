<p align="center">
  <a href="README.md">中文</a> | <a href="README_EN.md">English</a>
</p>

---

<h1 align="center">Not Good Review Master</h1>

<p align="center">A QQ group chatbot powered by AstrBot (official QQ bot) + LLM — @mention the bot to get AI-generated sharp reviews of group chat, ask questions, or dynamically add custom commands.</p>

## Features

- **@mention + keyword trigger**: @mention the bot with a keyword (e.g. "锐评下") to trigger LLM responses
- **Context-aware**: Uses recent group chat history as context for relevant, tailored responses — not random replies
- **Pluggable commands**: Add new keyword variants in `prompt_system.yaml` — no code changes needed
- **In-chat dynamic commands**: Add or delete custom keywords directly from the group chat via internal commands
- **Whitelist**: Only responds in configured group IDs
- **Local network only**: the AstrBot bridge pushes events to Go over local HTTP — no public IP required
- **Single binary deployment**: Compile to one executable, drop it on a server, and run
- **Persona system**: each command can carry a 5-dimension persona (identity/personality/relationship/emotion/system prompt) — replies have character but stay bounded
- **Direct Q&A on unknown commands**: when @mentioned but no keyword matches, the message is sent straight to the LLM for a plain reply (no persona)
- **Proactive replies**: when not mentioned, a lightweight LLM judge decides whether to join (off by default; cooldown/streak limits, serialized per bot+group)
- **Rich replies**: quote, @mention, image, voice, video and file segments; the QQ official bridge uploads media and prefers passive replies
- **Cost-optimized cache hits**: stateless requests + prefix caching, with an automatic extend/reset window decision based on token cost

## Architecture

```
QQ official bot ←→ AstrBot sidecar (OneBot 12 IM gateway) ←→ Go Bot ←→ LLM API
```

```
┌──────────┐  OneBot 12  ┌────────────────┐  events/actions  ┌──────────────────┐
│ QQ Group │ ←─────────→ │ AstrBot + bridge│ ←──────────────→ │  Go Bot          │
└──────────┘   (WS/HTTP)  └────────────────┘                  └────────┬─────────┘
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
IM ingress POST /im/event/{account} → provision/load workspace {platform}_{account}
  → queue (bot/ingest.go)
  → Dedup via O(1) message_id set
  → History comes from the shared state.db, filtered by workspace_id (no platform history action)
  → ProcessMessage (bot/handler.go)
     → Whitelist check
     → Truncate to max length
     → Store in ring buffer (zero-copy write)
     → @bot detection (mention.user_id == self.user_id + nickname fallback)
     → Prefix trie match → matched: dispatch to that command's handler (with its persona)
     → No match: fall back to a plain LLM reply (no persona)
```

## Quick Start

### Prerequisites

- An official QQ bot (AppID / AppSecret)
- [AstrBot](https://github.com/AstrBotDevs/AstrBot) with this repo's bridge plugin, `qq_official` adapter connected
- An OpenAI-compatible LLM API key (DeepSeek, Doubao, Tongyi Qianwen, etc.)

### Run from source

> Install [Go](https://go.dev/dl/) 1.25+ and [Node.js](https://nodejs.org/) 18+ first.

```bash
# Windows: double-click start_main.bat
# Linux/macOS: ./start_main.sh
# The script runs 3 steps automatically:
#   1. Install frontend deps + build frontend
#   2. Copy to embed directory
#   3. go run ./cmd/good-review
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
| `im.api_base` | Default outbound endpoint shared by all accounts | `http://127.0.0.1:6199` |
| `im.echo_self_messages` | Whether the implementation echoes the bot's own messages back as events (NapCat does, AstrBot bridge does not); when true, replies are stored via those events and not recorded locally | `false` |
| `im.accounts` | Optional per-account overrides `[{id, api_base}]`; `id` = `platform_account` | usually unset |
| `bot.allow_groups` | Allowed groups (`group_openid` for the official bot; `*` for all groups; accepts a comma-separated string or a YAML list) | `123456,789012` |
| `bot.allow_workspace_accounts` | Workspace IDs allowed to auto-provision (`qq_1906xxxx`; `*` for all; YAML list or comma-separated string) | `["qq_1906xxxx"]` |
| `bot.proactive_enabled` | Proactive replies: let the LLM decide whether to join when not mentioned | `false` |
| `bot.proactive_cooldown_sec` | Per-group cooldown after a proactive reply (0 = no cooldown) | `90` |
| `bot.proactive_max_streak` | Max consecutive proactive replies on one topic (0 = never) | `5` |
| `bot.proactive_judge_interval_sec` | Minimum interval between judge runs, measured from the previous start (0 = none) | `20` |
| `bot.proactive_judge_timeout_sec` | Judge timeout (timeout = no reply; 0 = use `runtime.llm_timeout_sec`) | `20` |
| `bot.proactive_judge_images` | Max images attached per judge (0 = image messages skipped) | `1` |
| `llm.provider` | Always `openai` (OpenAI-compatible) | `openai` |
| `llm.api_key` | LLM API key (lives in `secret.yaml`) | `sk-xxx` |
| `llm.api_base` | LLM API base URL | `https://api.deepseek.com` |
| `llm.model_name` | Model name | `deepseek-v4-flash` |
| `llm.temperature` | Sampling temperature (1.0=creative, 0.5=focused) | `1.2` |
| `llm.top_p` | Nucleus sampling (lower = more focused) | `0.95` |
| `llm.cache_hit_cost` | Cache hit cost (relative) | `0.033` |
| `llm.cache_miss_cost` | Cache miss cost (relative) | `1.0` |
| `llm.max_context_tokens` | Max context tokens per LLM request (force reset if exceeded) | `50000` |
| `llm.image_max` | Max images actually viewed per reply (0 = disabled) | `3` |
| `llm.reply_max_sentences` | Max sentences per final reply (one extra formatting call after drafting; 0 = disabled) | `5` |
| `llm.reply_rewrite_min_runes` | Drafts shorter than this many characters skip formatting (0 = no minimum) | `60` |
| `llm.image_local_dirs` | Local directories `view_image` may read (QQ official images are local AstrBot temp files) | `["~/.astrbot/data/temp"]` |
| `runtime.llm_send_count` | Recent messages sent per request (reset window) | `200` |
| `runtime.max_cache_msg` | Max ring-buffer messages (≈31×llm_send_count) | `6200` |
| `runtime.llm_timeout_sec` | LLM timeout (seconds) | `20` |
| `runtime.max_msg_rune` | Max characters per message | `500` |
| `runtime.reply_send_interval_ms` | Sentence send interval: >0 sends up to 5 messages one by one; 0 = single message | `800` |
| `runtime.im_listen_addr` | Ingress address for AstrBot-pushed events | `127.0.0.1:18090` |
| `runtime.web_port` | Web panel port (<=0 to disable) | `8080` |
| `runtime.web_username` | Web panel login username | `admin` |
| `runtime.web_password` | Web panel login password (required; lives in `secret.yaml`) | `"123456"` |

### Web Management Panel

Gin + JWT + embedded Vue SPA for group message monitoring.

| Method | Path | Auth | Description |
|---|---|---|---|
| POST | `/api/login` | No | Returns JWT token (validates username/password) |
| GET | `/api/status` | JWT | Nickname, Masked API Key, group count |
| GET | `/api/groups` | JWT | Per-group info with activity stats |
| GET | `/api/groups/:id` | JWT | Cached messages for one group |
| POST | `/api/logout` | JWT | No-op (stateless token) |

`web_username` / `web_password` are required whenever the web panel is enabled — there is no password-less mode (the bot exits at startup if they're missing). Set `web_port` to 0 or negative to disable the web panel entirely.

> **Secrets file**: `llm_api_key`, `web_password`, `jwt_secret`, `im_token`, `mcp_builtin_token` and
> `mcp_server_tokens` all live in a sibling `secret.yaml` (auto-created on first run, mode 0600, gitignored).
> Precedence is **env > secret.yaml**. Available env vars:
> `GOOD_REVIEW_LLM_API_KEY`, `GOOD_REVIEW_WEB_PASSWORD`, `GOOD_REVIEW_IM_TOKEN`.

### prompt_system.yaml

Commands are defined in list format. One command type can have multiple keyword + prompt variants:

```yaml
cmd:
  chat_review:            # Sends recent chat log to LLM
    - keyword: "猫娘"
      prompt: |
        You are a cute catgirl. Pick the cutest group member and compliment them.
      persona:            # optional: 5-dimension persona (all required once present)
        identity: |-
          A soft and clingy catgirl who loves being praised and nuzzles up to cute people.
        personality: '["clingy and affectionate","naively cheerful","melts when praised","protective but never holds a grudge"]'
        relationship: '{"role":"the group pet catgirl","approach":"sweet and cuddly with everyone"}'
        system_prompt: |-
          Be naturally affectionate without over-stuffing filler words; pick the cutest member and compliment them.
        emotion: '{"baseline":"joyful","reactivity":"high","expressiveness":"outward","recovery":"fast","style":"cute and playful"}'

rules:
  chat_review: |          # Shared rules appended to every chat_review prompt
    1. No personal attacks or prohibited content
    2. Keep under 100 characters
    3. Output the result directly, no extra explanation
    4. Pay more attention to the most recent 10 messages
```

Add a new variant by adding an entry under the same list — no code changes needed. If a `persona` block is present, all 5 fields are required; it is rendered at the tail of the user message sent to the LLM (see [Core Mechanisms](#core-mechanisms)).

`internal` holds the built-in features' own prompts. Edit them directly in `prompt_system.yaml`; hot reload picks them up. The first three are listed in the order they are assembled into the reply system prompt; the last three are standalone calls:

1. `internal.reply_requirement`: the 【要求】 line in the system prompt for @ replies
2. `internal.proactive_requirement`: the 【要求】 line used instead when replying proactively
3. `internal.focus_mention` / `internal.focus_last` / `internal.focus_context`: the focus hint appended at the end of the system prompt; a group mention always uses `focus_mention` (reply only to the last message that called the bot), private chat uses `focus_last`, proactive replies pick one via the judge's `focus`
4. `internal.proactive_judge`: the judge prompt that decides whether to reply proactively to a non-@ message; it outputs `reply / long_form / focus / reason` (never reply when the last message @s someone other than the bot)
5. `internal.add_persona`: system prompt used by `#添加人格` when generating a prompt + persona
6. `internal.reply_rewrite`: formatting prompt that rewrites the drafted reply into at most N short sentences (N = `llm.reply_max_sentences`); the request carries the bot identity, the chat log used to generate the reply and the reply itself, and asks the model to preserve the original tone

## Command System

### Two kinds of commands

| Kind | Defined in | Examples |
|---|---|---|
| **Function commands** | `prompt_system.yaml` / `prompt_custom.yaml` | `锐评下`, `猫娘` |
| **Internal commands** | Go code (`router/internal_cmd.go`) | `添加人格`, `删除人格`, `添加指令规则`, `删除指令规则`, `帮助` |

### Triggering

@mention the bot followed by a keyword. Extra text after the keyword is passed to the LLM as priority context:

```
@bot 猫娘 what do you think of the recent messages?
```

### Dynamic commands (from group chat)

Add a new persona directly from the group — the LLM generates the prompt and a 5-field persona:

```
@bot #添加人格(meanie-review)指令类型(chat_review)大模型想提示词(foul-mouthed, roasts everyone, calls them old)
```

Delete a persona:

```
@bot #删除人格(meanie-review)
```

System keywords (from `prompt_system.yaml`) and internal commands cannot be overwritten or deleted. Dynamically added personas are saved to `prompt_custom.yaml` and persist across restarts.

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

**2. Create a new handler file in `router/`** (e.g. `weather.go`), handler must be a Router method with the fixed signature `(event, groupID, systemPrompt, keywordPrompt, mentionerNick, extra, persona)`:

```go
func (r *Router) weatherHandler(event onebot.Event, groupID, systemPrompt, keywordPrompt, mentionerNick, extra, persona string) {
    r.Go(func(ctx context.Context) error {
        // Safe async: ctx auto-inherits shutdown signal (Ctrl+C cancels in-flight LLM calls)
        // systemPrompt  = bot identity (model name + nickname) + shared rules for this command
        // keywordPrompt = this command's prompt; persona = rendered 5-dim persona (empty if unset)
        // Build the user message: chat log + mentioner info + keyword prompt + persona
        userMsg := buildUserMsg(chatLog, mentionerNick, keywordPrompt, extra, persona)
        reply, err := r.llmClient.Review(ctx, userMsg, systemPrompt)
        ...
        return nil
    })
}
```

**3. Register in `handlerMap` in `router/command.go` `NewRouter()`**

```go
r.handlerMap = map[string]HandlerFunc{
    "chat_review": r.chatReview,
    "weather":     r.weatherHandler,  // add this line
}
```

## Project Structure

```
good-review-master/
├── cmd/
│   └── good-review/
│       └── main.go           # Entry point: logger, first-run prompt, app.Run + exit code
├── app/                     # Composition root: wires every component, starts/stops them
├── go.mod / go.sum           # Go module dependencies
├── config.yaml               # Live config (gitignored)
├── prompt_system.yaml        # System prompts (gitignored)
├── prompt_custom.yaml        # Dynamic prompts (gitignored, auto-created)
├── secret.yaml               # Secrets (API key / password / tokens; gitignored, mode 0600)
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
│   ├── client.go             # OneBot action client (resty, points at the AstrBot bridge)
│   ├── cqimage.go            # CQ-code parsing (image extraction, rich-segment placeholders, truncation)
│   └── types.go              # API data types
├── im/
│   └── server.go             # AstrBot event ingress (auth, decode, enqueue)
├── bot/
│   ├── ingest.go             # Event consumer + startup history backfill (context-aware)
│   └── handler.go            # Message processing: whitelist → @detection → routing
├── router/                   # Command system: prefix-trie matching + dispatch (was cmd/, package renamed)
│   ├── command.go            # Router + prefix trie matching + unmatched fallback + safe goroutine
│   ├── internal_cmd.go       # Internal commands (add/delete keyword, add/delete rule, help)
│   ├── chat_review.go        # Async chat_review handler (cache window + message assembly)
│   ├── chat_window.go        # Cache window decision: extend vs reset (pure function, unit-tested)
│   └── persona_render.go     # Persona rendering (5 dims → tail of user message)
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
main → app, config, apppath, logutil, version
app → config, llm, onebot, router, bot, web/server, mcpclient, mcpserver, logutil, internal/testutil
bot → config, cache, onebot, router
router → config, cache, llm, onebot, async
web/server → config, logutil, onebot, cache, version
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

`app` is the composition root and only `main` imports it; when a component needs another component, inject it in `app` rather than importing across packages.

## Logging

Logs are written to the `log/` directory under the working directory. Uses `zap` + `lumberjack`: size-based rotation at 20MB, max 30 backup files, 30-day retention, gzip-compressed old files. Logs go to both stdout (colored) and file.

## Deployment

- Local machine or cloud server — no public IP needed
- Compile to a single binary (frontend SPA embedded), no runtime dependencies
- **Auto-create config on first launch**: If `config.yaml` and `prompt_system.yaml` are missing, the exe auto-generates them from built-in templates. Edit and restart.
- Use `systemd` (Linux) or Task Scheduler (Windows) for auto-start on boot

## Testing

Three-layer testing:

1. **Unit tests**: `router/chat_window_test.go` — table-driven coverage of every branch of the cache-window decision (extend vs reset); the decision function is a pure function with explicit inputs and no global state.
2. **Test-mode infrastructure**: with `GOOD_REVIEW_TEST=1`, `FakeLLM` (fixed reply + records every call) replaces the real LLM, the AstrBot bridge uses the configured address, and `/api/debug/*` self-test endpoints (inject/reset/state/trigger) are registered — reachable only in test mode; in production the SPA fallback serves page HTML for those paths.
3. **E2E**: Playwright **API-level tests** (`request` fixture, no browser) driving the real test binary's HTTP API:

```bash
cd tests/e2e && pnpm test
```

- Covers: auth, group data, message data, the full bot flow (inject → trigger review → assert reply), and cache-hit cost (extend vs reset window).
- In-process cache/anchors are global, so tests run serially (`workers: 1`) and are isolated via `/api/debug/reset`; `trigger` blocks until the handler finishes (anchor updated) before returning, so async side effects never leak across tests.
- See [docs/testing.md](docs/testing.md) for details.

## Core Mechanisms

The project is designed around three goals: **concurrency safety, graceful shutdown, and testability**.

### Async & Concurrency Control

- **Goroutine pool** (`pool/`): fixed workers (default `runtime.NumCPU()*2`) with a bounded task queue; `Submit` is non-blocking and returns `false` when the queue is full (backpressure handled by the caller); `Shutdown` drains the queue gracefully. Pure stdlib, no internal deps.
- **Safe goroutine manager** (`async/`): `async.Group` wraps the pool and adds **automatic context propagation** (Ctrl+C cancels in-flight LLM calls) and **panic recovery** so a single panicking task can't take down the process; `Wait()` cancels first, then drains.
- **Single-writer architecture**: the poll loop is the only goroutine that writes to the cache, backed by `RWMutex` — virtually no write contention.

### Graceful Shutdown

`app.New` uses `signal.NotifyContext` to catch SIGINT/SIGTERM; all three layers consume the same lifecycle context (`app.App.ctx`):

```
signal → ctx cancelled → poll loop exits on select ctx.Done()
                       → webSrv.Shutdown(ctx) (10s timeout to drain requests)
                       → router.Wait() waits for in-flight goroutines (LLM calls cancellable)
```

Startup and shutdown order live in the `app` package: `app.New` only assembles (leaving `main.go` with logger setup, the first-run prompt, and exit codes), and `app.Run()` blocks until a signal, then shuts down in the order above.

### Cache Window Decision: Extend vs Reset (Cost Optimization)

Before every LLM call, the window to send is chosen by token cost (`router/chat_window.go`):

- **Extend** (cache hit): the `LLMAnchor{Start, LastSent}` anchor locates the last-sent window; cost = `hit prefix × cache_hit_cost + new messages × cache_miss_cost`;
- **Reset**: the most recent `llm_send_count` messages; cost = `everything × cache_miss_cost`.

Extend only when all three hold (anchor available + extend cheaper + within the `max_context_tokens` guardrail); otherwise reset. If the reset window exceeds the guardrail, messages are trimmed one by one from the oldest. Tokens are estimated heuristically (`cache/token.go`: CJK chars count 1 token each, ASCII 1 token per 4 chars) without building or allocating the string.

### Stateless Requests + Prefix Cache Hits

- **Stateless requests**: every LLM call (`llm.Review`) is a brand-new single `system + user` request — no server-side session is maintained. The "conversation feel" comes from the bot splicing that group's chat log into the user message; the request is stateless, but the prefix stays continuous.
- **Prefix cache hits**: DeepSeek automatically disk-caches request prefixes that match exactly from token 0 (content-addressed, per-user, best-effort); the hit portion is billed at `cache_hit_cost` and reuses cached computation. When the window is extended, the head of the new request is byte-identical to the last one → hits, and only the appended messages are charged as a miss.
- **Per-group isolation**: both the ring cache and the extend anchor are keyed by `groupID`; every group maintains its own chat log and window — different groups have different prefixes that occupy separate cache units without evicting each other (content-addressed, not "one cache slot overwritten by another").
- **The key to not breaking hits**: the persona is rendered at the **tail** of the user message (below), keeping `system + chat-log prefix` stable when switching commands or personas. A group only loses hits when its own prefix changes (forced reset, or the cache is cleared after long inactivity).

### Persona System

Every command can carry a 5-dimension persona (`Persona` in `config/prompt.go`), all fields required:

| Field | Type | Description |
| --- | --- | --- |
| `identity` | text | Background identity (name/age/job/history, concrete and believable) |
| `personality` | JSON array | Personality traits (behavioral descriptions) |
| `relationship` | JSON object | Relationship with group members (role / how to treat them) |
| `system_prompt` | text | Persona-level system directive (behavior boundaries / output requirements) |
| `emotion` | JSON object | Emotion dimensions (dimension → value, the core of the persona) |

**Rendering position**: `RenderPersona` (`router/persona_render.go`) renders the persona as a segment at the **tail of the system prompt** (after the identity header); the user message (chat log) does not carry the persona block.

**Stateless persona**: no persona state is maintained; the chat window is the implicit state, and the LLM is the final interpreter of both dimension changes and the answer.

### Data Structures

- **Ring-buffer cache** (`cache/`): fixed-size array + write pointer, overwrites the oldest when full — **zero-copy writes**; `msgIDSet` gives O(1) dedup; `GetAll()` splices two segments to preserve time order.
- **Prefix-trie routing** (`router/command.go`): `trieMatch` walks chars and returns the **longest matching prefix** ("锐评下" wins over "锐评"), O(k) regardless of route count; user commands are rebuilt dynamically from YAML, internal commands persist.

### Engineering Robustness

- **Explicit dependency injection, zero `init()` side effects**: all components are constructed top-down in `main()`.
- **Defenses**: group whitelist, per-message rune truncation, @detection via the standard mention with unique-nickname fallback, auto-generate missing config from embedded templates then exit.
- **Observability**: zap + lumberjack dual output (console + 20MB-rotating file, 30-day retention, gzip).
- **Web panel**: SPA embedded via `//go:embed`, JWT (HS256) auth, no password-less mode.
