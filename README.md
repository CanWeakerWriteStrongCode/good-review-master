<p align="center">
  <a href="#中文">中文</a> | <a href="#english">English</a>
</p>

---

<h1 id="中文" align="center">🔪 不是好评大师</h1>

<p align="center">QQ 群聊锐评机器人 —— 基于 NapCatQQ + 大模型，@一下即可锐评群友、问答互动</p>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.25+-00ADD8?style=flat&logo=go" alt="Go version">
  <img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="License">
  <img src="https://img.shields.io/badge/QQ-NapCatQQ-12B7F5?style=flat" alt="NapCatQQ">
</p>

## 📑 目录

- [特性](#-特性)
- [架构](#-架构)
- [快速开始](#-快速开始)
- [配置说明](#-配置说明)
- [项目结构](#-项目结构)
- [扩展新命令](#-扩展新命令)
- [部署说明](#-部署说明)

## ✨ 特性

- **@机器人 + 关键词触发**：群内 @机器人并说出关键词（如"锐评下"），即可触发大模型生成回复
- **群聊上下文感知**：基于最近的群聊记录生成上下文相关的锐评，不是无脑随机回复
- **插件式命令扩展**：在 `prompt.yaml` 里加配置 + `cmd/` 下写 handler → 一行路由注册，即可新增命令
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
- [NapCatQQ](https://github.com/NapNeko/NapCatQQ) 已安装并登录（HTTP 服务已开启）
- 一个 OpenAI 兼容的大模型 API Key（DeepSeek、豆包、通义千问等均可）

### 代码启动

```bash
# 1. 克隆仓库
git clone https://github.com/your-username/good-review-master.git
cd good-review-master

# 2. 复制创建配置文件config_example.yaml 名字改成 config.yaml
cp config_example.yaml config.yaml

# 3. 编辑 config.yaml，填入你的配置（详见下方配置说明），编辑prompt.yaml可以修改不同功能的提示词

# 4. 运行
# Windows：双击 start_main.bat
# Linux/macOS：./start_main.sh

# 或者直接 go run
go run .
```

### 打包为可执行文件启动

```bash
# Windows：双击 build_exe.bat
# Linux：./build_linux.sh
# config.yaml 配置和 prompt.yaml 配置放到exe同目录下，运行即可
```

## ⚙ 配置说明

| 配置项 | 说明 | 示例值 |
|--------|------|--------|
| `napcat.http_api` | NapCatQQ HTTP API 地址 | `http://127.0.0.1:3000` |
| `napcat.access_token` | NapCatQQ 访问令牌（WebUI 中设置） | `""` |
| `bot.qq` | 机器人 QQ 号 | `123456` |
| `bot.allow_groups` | 允许响应的群号（逗号分隔） | `123456,789012` |
| `llm.provider` | 固定填 `openai`（兼容所有 OpenAI 格式） | `openai` |
| `llm.api_key` | 大模型 API Key | `sk-xxx` |
| `llm.api_base` | 大模型接口地址 | `https://api.deepseek.com` |
| `llm.model_name` | 模型名称 | `deepseek-v4-flash` |
| `llm.temperature` | 锐评风格：0.8=犀利 0.5=温和 | `0.8` |
| `runtime.max_cache_msg` | 缓存消息数 | `30` |
| `runtime.llm_timeout_sec` | 大模型超时（秒） | `20` |
| `runtime.max_msg_rune` | 单条消息最大字符数 | `200` |
| `runtime.poll_interval_sec` | 轮询间隔（秒） | `3` |

### 命令提示词配置

命令提示词单独存放在 `prompt.yaml` 中（不常变动）。

## 📁 项目结构

```
good-review-master/
├── main.go              # 入口：初始化配置、大模型客户端，启动轮询
├── config/
│   ├── config.go        # 运行时配置加载（config.yaml → struct）
│   └── prompt.go       # 提示词配置加载（prompt.yaml → struct）
├── cache/
│   └── cache.go         # 消息环形缓冲区（按群维度、去重）
├── llm/
│   └── llm.go           # 大模型客户端接口（OpenAI 兼容）
├── onebot/
│   ├── client.go        # NapCatQQ HTTP API 客户端
│   └── types.go         # API 数据类型定义
├── bot/
│   ├── polling.go       # 轮询拉取消息 + 去重
│   └── handler.go       # 消息处理：白名单检查 → emoji过滤 → 命令路由
├── cmd/
│   ├── router.go        # 命令路由表
│   ├── sharptake.go     # "锐评下" 命令
│   └── whoami.go        # "你是谁" 命令
├── config_example.yaml  # 运行时配置模板
├── config.yaml          # 运行时配置（gitignore）
├── prompt.yaml         # 提示词配置
├── start_main.bat       # Windows 启动脚本
├── start_main.sh        # Linux 启动脚本
├── build_exe.bat        # Windows 编译脚本
└── build_linux.sh       # Linux 编译脚本
```

### 包依赖关系

```
main → config, llm, bot
bot → config, cache, onebot, cmd
cmd → config, cache, llm, onebot
onebot → config
cache → config
llm → (无内部依赖)
config → (无内部依赖)
```

## ➕ 扩展新命令

三步完成，无需改动 `config/config.go`：

**1. 在 `prompt.yaml` 的 `cmd:` 下添加配置**

```yaml
cmd:
  weather:
    keyword: "天气"
    prompt: "你是天气助手..."
```

**2. 在 `cmd/` 下新建 handler 文件**（如 `weather.go`）

```go
func weather(event onebot.Event, groupID string, prompt string) {
    go func() {
        // 调用 llm.DefaultClient.Review(ctx, chatLog, prompt)
        // 或发送静态回复
    }()
}
```

**3. 在 `cmd/router.go` 注册路由**

```go
{
    Keyword: config.CmdConfigs["weather"].Keyword,
    Prompt:  config.CmdConfigs["weather"].Prompt,
    Handler: weather,
},
```

## 🌐 部署说明

- 本机或云服务器均可，无需公网 IP
- NapCatQQ 和 Go Bot 部署在同一台机器上，纯内网 HTTP 通信
- 编译为单二进制文件，无运行时依赖，丢上去就跑
- 推荐使用 `systemd`（Linux）或任务计划程序（Windows）设为开机自启

---

<h1 id="english" align="center">🔪 Not Good Review Master</h1>

<p align="center">A QQ group chat bot that listens to messages via NapCatQQ and triggers AI-generated sharp reviews through keyword matching.</p>

## Quick Start

```bash
git clone https://github.com/your-username/good-review-master.git
cd good-review-master
cp config_example.yaml config.yaml
# Edit config.yaml and prompt.yaml with your settings, then:
go run .
```

## Configuration

| Key | Description |
|-----|-------------|
| `napcat.http_api` | NapCatQQ HTTP API address |
| `napcat.access_token` | NapCatQQ access token |
| `bot.qq` | Your bot's QQ number |
| `bot.allow_groups` | Allowed group IDs (comma-separated) |
| `llm.api_key` | LLM API key |
| `llm.api_base` | LLM API base URL |
| `llm.model_name` | Model name |
| `llm.temperature` | Review sharpness (0.8=sharp, 0.5=mild) |

## Project Structure

```
main.go          - Entry point
config/          - Configuration loader
cache/           - Message ring buffer cache
llm/             - LLM client (OpenAI-compatible)
onebot/          - NapCatQQ HTTP API client
bot/             - Message filtering + polling loop
cmd/             - Command router + handlers
```

## Requirements

- Go 1.25+
- NapCatQQ running locally
- An OpenAI-compatible LLM API (DeepSeek, etc.)
