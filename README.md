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
- [扩展新指令](#-扩展新指令)
- [部署说明](#-部署说明)

## ✨ 特性

- **@机器人 + 关键词触发**：群内 @机器人并说出关键词（如"锐评下"），即可触发大模型生成回复
- **群聊上下文感知**：基于最近的群聊记录生成上下文相关的锐评，不是无脑随机回复
- **插件式指令扩展**：在 `prompt_system.yaml` 里加配置即可新增同类型指令变体，无需改代码
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

# 3. 编辑 config.yaml，填入你的配置（详见下方配置说明），编辑prompt_system.yaml可以修改不同功能的提示词

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
# config.yaml 配置和 prompt_system.yaml 配置放到exe同目录下，运行即可
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
| `llm.top_p` | 核采样参数：越小输出越集中 | `0.9` |
| `runtime.max_cache_msg` | 缓存消息数 | `30` |
| `runtime.llm_timeout_sec` | 大模型超时（秒） | `20` |
| `runtime.max_msg_rune` | 单条消息最大字符数 | `200` |
| `runtime.poll_interval_sec` | 轮询间隔（秒） | `3` |

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
```

新增同类型变体只需在对应列表下加一项，无需改 Go 代码。

### 群内动态添加指令

在群里 @机器人 发送以下格式，即可动态添加指令到 `prompt_custom.yaml`，重启后依然生效：

```
@机器人 添加指令(类型)关键字(关键词)大模型想提示词(要点)   # 让大模型帮忙写提示词
```

示例：
```
@机器人 添加指令(chat_review)关键字(雌小鬼锐评下)大模型想提示词(嘴臭的雌小鬼，毒舌，喜欢说老登)
```

添加后立即生效，无需重启。动态添加的指令保存在 `prompt_custom.yaml`，与 `prompt_system.yaml` 分离。

## 📁 项目结构

```
good-review-master/
├── main.go              # 入口：初始化配置、大模型客户端，启动轮询
├── config/
│   ├── config.go        # 运行时配置加载（config.yaml → struct）
│   └── prompt.go       # 提示词配置加载（prompt_system.yaml → struct）
├── cache/
│   └── cache.go         # 消息环形缓冲区（按群维度、去重）
├── llm/
│   └── llm.go           # 大模型客户端接口（OpenAI 兼容）
├── onebot/
│   ├── client.go        # NapCatQQ HTTP API 客户端
│   └── types.go         # API 数据类型定义
├── bot/
│   ├── polling.go       # 轮询拉取消息 + 去重
│   └── handler.go       # 消息处理：白名单检查 → emoji过滤 → 指令路由
├── cmd/
│   ├── router.go        # 指令路由表（动态生成 + 系统路由）
│   ├── internal_cmd.go  # 内部指令（添加指令、查看指令列表等）
│   ├── chat_review.go   # chat_review 处理函数
├── config_example.yaml  # 运行时配置模板
├── config.yaml          # 运行时配置（gitignore）
├── prompt_system.yaml          # 提示词配置
├── prompt_custom.yaml   # 动态添加的提示词（gitignore，程序自动创建）
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

**2. 在 `cmd/` 下新建 handler 文件**（如 `weather.go`）

```go
func weather(event onebot.Event, groupID string, prompt string) {
    go func() {
        // 调用 llm.DefaultClient.Review(ctx, chatLog, prompt)
        // 或发送静态回复
    }()
}
```

**3. 在 `cmd/router.go` 的 `handlerMap` 注册**

```go
var handlerMap = map[string]func(onebot.Event, string, string){
    "chat_review": sharpTake,
    "weather":     weather,  // 新增这一行
}
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
# Edit config.yaml and prompt_system.yaml with your settings, then:
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
| `llm.top_p` | Nucleus sampling: lower = more focused |

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
