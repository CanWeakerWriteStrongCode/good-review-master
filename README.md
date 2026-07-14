<p align="center">
  <a href="#中文">中文</a> | <a href="#english">English</a>
</p>

---

<h1 id="中文" align="center">🔪 不是好评大师</h1>

<p align="center">QQ 群聊锐评机器人 —— 基于 NapCatQQ + 大模型，@一下即可锐评群友、问答互动</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go" alt="Go version">
  <img src="https://img.shields.io/badge/Node.js-18+-339933?style=flat&logo=nodedotjs" alt="Node.js">
  <img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License">
  <img src="https://img.shields.io/badge/QQ-NapCatQQ-12B7F5?style=flat" alt="NapCatQQ">
</p>

## 📑 目录

- [特性](#-特性)
- [架构](#-架构)
- [快速开始](#-快速开始)
- [配置说明](#-配置说明)
- [项目结构](#-项目结构)
- [扩展新指令](#-扩展新指令)
- [部署说明](#-部署说明)

## ✨ 特性

- **@机器人 + 关键词触发**：群内 @机器人并说出关键词（如"锐评下"），即可触发大模型生成回复
- **群聊上下文感知**：基于最近的群聊记录生成上下文相关的锐评，不是无脑随机回复
- **插件式指令扩展**：在 `prompt_system.yaml` 里加配置即可新增同类型指令变体，无需改代码
- **群内动态添加指令**：在群里 @机器人即可动态增删自定义指令，无需重启
- **白名单机制**：只响应指定群号，安全可控
- **纯内网通信**：Go 后端通过 HTTP 轮询 NapCatQQ 本地 API，无需公网 IP
- **单二进制部署**：编译为单个可执行文件，丢到服务器上就能跑

## 🏗 架构

```
QQ ←→ NapCatQQ (本地 HTTP API) ←→ Go Bot (轮询) ←→ LLM API (OpenAI 兼容)
```

```
┌──────────┐     HTTP      ┌────────────┐     HTTP      ┌──────────┐
│   QQ 群   │ ←──────────→ │  NapCatQQ   │ ←──────────→ │  Go Bot  │
└──────────┘               └────────────┘               └─────┬────┘
                                                              │
                                                              │ OpenAI API
                                                              ▼
                                                      ┌──────────────┐
                                                      │  LLM 大模型   │
                                                      │ (DeepSeek等)  │
                                                      └──────────────┘
```

## 🚀 快速开始

### 前置条件

- Go 1.25+
- Node.js 18+
- [Git](https://git-scm.com/)（打包时自动提取版本号）
- [NapCatQQ](https://github.com/NapNeko/NapCatQQ) 已安装并登录（HTTP 服务已开启）
- 一个 OpenAI 兼容的大模型 API Key（DeepSeek、豆包、通义千问等均可）

### 代码启动

> 请先安装 [Go](https://go.dev/dl/) 1.25+ 和 [Node.js](https://nodejs.org/) 18+。

```bash
# Windows：双击 start_main.bat
# Linux/macOS：./start_main.sh
# 首次运行会自动创建 config.yaml 并退出
# 编辑 config.yaml 填入你的配置，重新运行 exe 即可
```

### 打包为可执行文件启动

```bash
# Windows：双击 build_exe.bat
# Linux/macOS：./build_linux.sh
# 脚本会自动交叉编译，在 dist/ 下生成 4 个平台的可执行文件：
#   good-review-master-windows-amd64.exe
#   good-review-master-linux-amd64
#   good-review-master-darwin-amd64     (Intel Mac)
#   good-review-master-darwin-arm64     (Apple Silicon)
# 将对应平台的文件复制到任意目录运行，首次运行会自动创建 config.yaml 并退出
# 编辑 config.yaml 填入你的配置，重新运行即可
```

## ⚙ 配置说明

| 配置项                         | 说明                           | 示例值                        |
|-----------------------------|------------------------------|----------------------------|
| `napcat.http_api`           | NapCatQQ HTTP API 地址         | `http://127.0.0.1:3000`    |
| `napcat.access_token`       | NapCatQQ 访问令牌（WebUI 中设置）     | `""`                       |
| `bot.qq`                    | 机器人 QQ 号                     | `123456`                   |
| `bot.allow_groups`          | 允许响应的群号（逗号分隔）                | `123456,789012`            |
| `llm.provider`              | 固定填 `openai`（兼容所有 OpenAI 格式） | `openai`                   |
| `llm.api_key`               | 大模型 API Key                  | `sk-xxx`                   |
| `llm.api_base`              | 大模型接口地址                      | `https://api.deepseek.com` |
| `llm.model_name`            | 模型名称                         | `deepseek-v4-flash`        |
| `llm.temperature`           | 锐评风格：1.0=发散 0.5=集中           | `1.2`                      |
| `llm.top_p`                 | 核采样参数：越小输出越集中                | `0.95`                     |
| `runtime.max_cache_msg`     | 缓存消息数                        | `20`                       |
| `runtime.llm_timeout_sec`   | 大模型超时（秒）                     | `20`                       |
| `runtime.max_msg_rune`      | 单条消息最大字符数                    | `200`                      |
| `runtime.poll_interval_sec` | 轮询间隔（秒）                      | `3`                        |

### 指令提示词配置

指令提示词存放在 `prompt_system.yaml` 中，采用 list 格式，同一类型指令可配多个 keyword + prompt 变体：

```yaml
cmd:
  chat_review:            # 形式：发送最近群聊记录给大模型
    - keyword: "锐评下"
      prompt: |
        你是群聊毒舌锐评机器人。
        规则：...
    - keyword: "猫娘来看看"
      prompt: |
        你是一只可爱的猫娘...

rules:
  chat_review: |          # 共享规则：追加到每条 chat_review 指令的 prompt 末尾
    1. 禁止人身攻击和违禁内容
    2. 字数控制在 100 字以内
    3. 直接输出结果，不额外解释
    4. 重点关注最近 10 条消息
```

新增同类型变体只需在对应列表下加一项，无需改 Go 代码。

### 群内动态添加指令

在群里 @机器人 发送以下格式，即可动态添加指令到 `prompt_custom.yaml`，重启后依然生效：

```
@机器人 添加关键字(关键词)指令(指令类型)大模型想提示词(要点)
```

示例：
```
@机器人 添加关键字(雌小鬼锐评下)指令(chat_review)大模型想提示词(嘴臭的雌小鬼，毒舌，喜欢说老登)
```

添加后立即生效，无需重启。动态添加的指令保存在 `prompt_custom.yaml`，与 `prompt_system.yaml` 分离。

通过群内指令只能增删 `prompt_custom.yaml` 中的自定义指令，无法修改或删除 `prompt_system.yaml` 中的系统指令和内置指令（如 `帮助`、`添加关键字`）。

## 📁 项目结构

```
good-review-master/
├── main.go                  # 入口：初始化配置、LLM 客户端，启动轮询 + 优雅退出
├── go.mod / go.sum           # Go 模块依赖
├── config.yaml               # 运行时配置（gitignore）
├── prompt_system.yaml        # 系统提示词配置（gitignore）
├── prompt_custom.yaml        # 动态添加的提示词（gitignore，程序自动创建）
├── start_main.bat / .sh      # 开发启动脚本
├── build_exe.bat / .sh       # 编译打包脚本
├── config/
│   ├── config.go             # 运行时配置加载（config.yaml → struct）
│   ├── config_example.yaml   # 内置配置模板（编译时嵌入 exe）
│   ├── embed.go              # //go:embed 模板嵌入
│   └── prompt.go             # 提示词配置加载+热重载+增删改
├── cache/
│   └── cache.go              # 环形缓冲区（零拷贝写入，O(1) 去重）
├── llm/
│   └── llm.go                # 大模型客户端（go-openai SDK，连接池，类型安全）
├── async/
│   └── async.go             # 安全 goroutine 管理器（errgroup + ctx 自动传播 + panic recover）
├── logutil/
│   └── logger.go             # 日志（zap + lumberjack，20MB 切割，30 天保留）
├── onebot/
│   ├── client.go             # NapCatQQ HTTP API 客户端（resty，自动序列化+重试）
│   └── types.go              # API 数据类型定义
├── bot/
│   ├── polling.go            # 轮询拉取消息 + 去重（context 支持优雅退出）
│   └── handler.go            # 消息处理：白名单 → @检测 → 指令路由
└── cmd/
    ├── command.go            # Router + 前缀树(trie)路由匹配 + 安全 goroutine 启动
    ├── internal_cmd.go        # 内部指令（添加关键字、删除关键字、帮助）
    └── chat_review.go         # chat_review 异步处理函数
```

### 包依赖关系

```
main → config, llm, logutil, bot, onebot, async
bot → config, cache, onebot, cmd
cmd → config, cache, llm, onebot, async
async → logutil
onebot → (无内部依赖)
cache → (无内部依赖)
llm → (无内部依赖)
config → apppath
logutil → apppath
apppath → (无内部依赖)
```

## ➕ 扩展新指令

### 在 prompt_system.yaml 中添加同类型变体（无需改代码）

在 `chat_review` 列表下新增一项即可：

```yaml
cmd:
  chat_review:
    - keyword: "雌小鬼锐评下"
      prompt: |
        你是嘴臭的雌小鬼...
```

### 新增指令类型（需写代码）

三步完成：

**1. 在 `prompt_system.yaml` 的 `cmd:` 下添加新类型配置**

```yaml
cmd:
  weather:
    - keyword: "天气"
      prompt: "你是天气助手..."
```

**2. 在 `cmd/` 下新建 handler 文件**（如 `weather.go`），handler 为 Router 的方法：

```go
func (r *Router) weatherHandler(event onebot.Event, groupID string, prompt string) {
    r.Go(func(ctx context.Context) error {
        // 异步安全启动：ctx 自动继承 shutdown 信号
        reply, err := r.llmClient.Review(ctx, chatLog, prompt)
        ...
        return nil
    })
}
```

**3. 在 `cmd/command.go` 的 `NewRouter()` 中注册到 `handlerMap`**：

```go
r.handlerMap = map[string]HandlerFunc{
    "chat_review": r.chatReview,
    "weather":     r.weatherHandler,  // 新增这一行
}
```

## 🌐 部署说明

- 本机或云服务器均可，无需公网 IP
- NapCatQQ 和 Go Bot 部署在同一台机器上，纯内网 HTTP 通信
- 编译为单二进制文件，无运行时依赖，丢上去就跑
- **首次启动自动创建 config.yaml**：exe 运行时若同目录没有 `config.yaml`，自动从内置模板生成一份，编辑后重新运行即可。无需手动准备 `config_example.yaml`
- 推荐使用 `systemd`（Linux）或任务计划程序（Windows）设为开机自启

---

<h1 id="english" align="center">🔪 Not Good Review Master</h1>

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

- Go 1.25+
- Node.js 18+
- [Git](https://git-scm.com/) (auto-extracts version tag during build)
- [NapCatQQ](https://github.com/NapNeko/NapCatQQ) installed and logged in (HTTP service enabled)
- An OpenAI-compatible LLM API key (DeepSeek, Doubao, Tongyi Qianwen, etc.)

### Run from source

> Install [Go](https://go.dev/dl/) 1.25+ and [Node.js](https://nodejs.org/) 18+ first.

```bash
# Windows: double-click start_main.bat
# Linux/macOS: ./start_main.sh
# First run auto-creates config.yaml.
# Edit config.yaml with your settings, then run again.
```

### Run compiled exe

Drop the exe in an empty directory and run it. On first launch, if `config.yaml` is missing, the exe auto-creates it from a built-in template. Edit the generated `config.yaml` and restart.

### Build executable

```bash
# Windows: double-click build_exe.bat
# Linux/macOS: ./build_linux.sh
# Cross-compiles to 4 targets under dist/:
#   good-review-master-windows-amd64.exe
#   good-review-master-linux-amd64
#   good-review-master-darwin-amd64     (Intel Mac)
#   good-review-master-darwin-arm64     (Apple Silicon)
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
| `runtime.max_cache_msg` | Max cached messages per group | `20` |
| `runtime.llm_timeout_sec` | LLM timeout (seconds) | `20` |
| `runtime.max_msg_rune` | Max characters per message | `200` |
| `runtime.poll_interval_sec` | Poll interval (seconds) | `3` |

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
├── start_main.bat / .sh      # Dev launcher scripts
├── build_exe.bat / .sh       # Build & package scripts
├── config/
│   ├── config.go             # Runtime config (config.yaml → struct)
│   ├── config_example.yaml   # Built-in config template (embedded at build time)
│   ├── embed.go              # //go:embed template embedding
│   └── prompt.go             # Prompt config loading + hot-reload + CRUD
├── cache/
│   └── cache.go              # Per-group ring buffer (zero-copy writes, O(1) dedup)
├── llm/
│   └── llm.go                # LLM client (go-openai SDK, connection pooling, typed)
├── async/
│   └── async.go             # Safe goroutine manager (errgroup + auto ctx + panic recover)
├── logutil/
│   └── logger.go             # Logging (zap + lumberjack, 20MB rotation, 30-day retention)
├── onebot/
│   ├── client.go             # NapCatQQ HTTP API client (resty, auto-marshal + retry)
│   └── types.go              # API data types
├── bot/
│   ├── polling.go            # HTTP poll loop + history fetching (context-aware)
│   └── handler.go            # Message processing: whitelist → @detection → routing
└── cmd/
    ├── command.go            # Router + prefix trie matching + safe goroutine launch
    ├── internal_cmd.go       # Internal commands (add/delete keyword, help)
    └── chat_review.go        # Async chat_review handler
```

## Logging

Logs are written to the `log/` directory under the working directory. Uses `zap` + `lumberjack`: size-based rotation at 20MB, max 30 backup files, 30-day retention, gzip-compressed old files. Logs go to both stdout (colored) and file.

## Deployment

- Local machine or cloud server — no public IP needed
- NapCatQQ and Go Bot run on the same machine, communicate via local HTTP
- Compile to a single binary, no runtime dependencies
- **Auto-create config on first launch**: If `config.yaml` is missing, the exe auto-generates one from its built-in template. Edit it and restart. No need to manually prepare `config_example.yaml`.
- Use `systemd` (Linux) or Task Scheduler (Windows) for auto-start on boot
