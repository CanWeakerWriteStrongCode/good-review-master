<p align="center">
  <a href="README.md">中文</a> | <a href="README_EN.md">English</a>
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
- [测试](#-测试)
- [核心机制](#-核心机制)

## ✨ 特性

- **@机器人 + 关键词触发**：群内 @机器人并说出关键词（如"锐评下"），即可触发大模型生成回复
- **群聊上下文感知**：基于最近的群聊记录生成上下文相关的锐评，不是无脑随机回复
- **插件式指令扩展**：在 `prompt_system.yaml` 里加配置即可新增同类型指令变体，无需改代码
- **群内动态添加指令**：在群里 @机器人即可动态增删自定义指令，无需重启
- **白名单机制**：只响应指定群号，安全可控
- **纯内网通信**：Go 后端通过 HTTP 轮询 NapCatQQ 本地 API，无需公网 IP
- **单二进制部署**：编译为单个可执行文件，丢到服务器上就能跑
- **人格系统**：每条指令可配 5 维人格（身份/性格/关系/情绪/系统指令），回复有性格但有边界
- **@机器人直接问答**：@机器人但没匹配到指令时，直接把消息发给大模型回答（不带人格）
- **成本优化缓存命中**：无状态请求 + 前缀缓存，按 token 成本自动扩展/重置窗口，省钱省算力

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

- [NapCatQQ](https://github.com/NapNeko/NapCatQQ) 已安装并登录（HTTP 服务已开启）
- 一个 OpenAI 兼容的大模型 API Key（DeepSeek、豆包、通义千问等均可）

### 代码启动

> 请先安装 [Go](https://go.dev/dl/) 1.25+ 和 [Node.js](https://nodejs.org/) 18+。

```bash
# Windows：双击 start_main.bat
# Linux/macOS：./start_main.sh
# 脚本会自动完成 3 步：
#   1. 安装前端依赖 + 构建前端
#   2. 拷贝到嵌入目录
#   3. go run main.go 启动服务
# 首次运行会自动创建 config.yaml 并退出
# 编辑 config.yaml 填入你的配置，重新运行即可
```

### 打包为可执行文件启动

> 打包还需要 [Git](https://git-scm.com/)，用于自动提取版本号。

```bash
# Windows：双击 build_exe.bat
# Linux/macOS：./build_linux.sh
# 脚本会自动完成 3 步：
#   1. 安装前端依赖 + 构建前端
#   2. 拷贝到嵌入目录
#   3. 交叉编译 4 个平台的可执行文件到 dist/ 目录
#
# 版本号自动从 git tag 提取，无需手动维护：
#   打了 tag v1.0.0 → 文件名带 v1.0.0
#   没打 tag  → 文件名带 abc1234（commit hash）
#   有未提交修改 → 文件名带 abc1234-dirty
#
# 输出示例：
#   dist/good-review-master-windows-amd64-v1.0.0.exe
#   dist/good-review-master-linux-amd64-v1.0.0
#   dist/good-review-master-darwin-amd64-v1.0.0     (Intel Mac)
#   dist/good-review-master-darwin-arm64-v1.0.0     (Apple Silicon)
# 将对应平台的文件复制到任意目录运行，首次运行会自动创建 config.yaml 并退出
# 编辑 config.yaml 填入你的配置，重新运行即可
```

## ⚙ 配置说明

| 配置项                         | 说明                              | 示例值                        |
|-----------------------------|---------------------------------|----------------------------|
| `napcat.http_api`           | NapCatQQ HTTP API 地址            | `http://127.0.0.1:3000`    |
| `napcat.access_token`       | NapCatQQ 访问令牌（WebUI 中设置）        | `""`                       |
| `bot.qq`                    | 机器人 QQ 号                        | `123456`                   |
| `bot.allow_groups`          | 允许响应的群号（逗号分隔）                   | `123456,789012`            |
| `llm.provider`              | 固定填 `openai`（兼容所有 OpenAI 格式）    | `openai`                   |
| `llm.api_key`               | 大模型 API Key                     | `sk-xxx`                   |
| `llm.api_base`              | 大模型接口地址                         | `https://api.deepseek.com` |
| `llm.model_name`            | 模型名称                            | `deepseek-v4-flash`        |
| `llm.temperature`           | 锐评风格：1.0=发散 0.5=集中              | `1.2`                      |
| `llm.top_p`                 | 核采样参数：越小输出越集中                   | `0.95`                     |
| `llm.cache_hit_cost`        | 缓存命中单价（相对值）                     | `0.033`                    |
| `llm.cache_miss_cost`       | 缓存未命中单价（相对值）                    | `1.0`                      |
| `llm.max_context_tokens`    | 单次发送大模型的上下文 token 上限（超限强制重置）    | `50000`                    |
| `runtime.llm_send_count`    | 每次发送的最近消息条数（重置窗口）               | `200`                      |
| `runtime.max_cache_msg`     | 环形缓冲条数上限（建议 ≈31×llm_send_count） | `6200`                     |
| `runtime.llm_timeout_sec`   | 大模型超时（秒）                        | `20`                       |
| `runtime.max_msg_rune`      | 单条消息最大字符数                       | `500`                      |
| `runtime.poll_interval_sec` | 轮询间隔（秒）                         | `3`                        |
| `runtime.web_port`          | Web 管理面板端口（<=0 禁用）              | `8080`                     |
| `runtime.web_username`      | Web 面板登录用户名                     | `admin`                    |
| `runtime.web_password`      | Web 面板登录密码（必填）                  | `"123456"`                 |

### Web 管理面板

Gin + JWT + 内嵌 Vue SPA，提供群消息监控页面。

| 方法 | 路径 | 鉴权 | 说明 |
|---|---|---|---|
| POST | `/api/login` | 否 | 返回 JWT token（账号密码校验） |
| GET | `/api/status` | JWT | BotQQ、昵称、脱敏 API Key、群数量 |
| GET | `/api/groups` | JWT | 各群信息 + 活跃状态 |
| GET | `/api/groups/:id` | JWT | 单群缓存消息 |
| POST | `/api/logout` | JWT | 登出（无状态 token，占位） |

`web_password` 必填，登录页面始终需要账号密码。`web_port` 设为 0 或负数可以完全禁用 Web 面板。

### 指令提示词配置

指令提示词存放在 `prompt_system.yaml` 中，采用 list 格式，同一类型指令可配多个 keyword + prompt 变体：

```yaml
cmd:
  chat_review:            # 形式：发送最近群聊记录给大模型
    - keyword: "猫娘"
      prompt: |
        你是一只可爱的猫娘，在群聊里撒娇。基于群聊记录，选出你觉得最可爱的一位群友，用符合猫娘气质的语气夸夸TA、撒个娇。
      persona:            # 可选：5 维人格（配了就全必填）
        identity: |-
          一只软萌的猫娘，粘人爱撒娇，最喜欢被夸，看到可爱的人会忍不住想贴贴。
        personality: '["爱撒娇粘人","单纯容易开心","被夸会炸毛","护短但不记仇"]'
        relationship: '{"角色":"群里的团宠猫娘","对待方式":"对谁都软乎乎地撒娇"}'
        system_prompt: |-
          撒娇自然、不过度堆砌语气词；选出最可爱的一位群友夸夸TA。
        emotion: '{"情绪底色":"愉悦","情绪反应性":"高","情绪外显度":"外放","恢复速度":"快","表达方式":"撒娇卖萌"}'

rules:
  chat_review: |          # 共享规则：追加到每条 chat_review 指令的 prompt 末尾
    1. 禁止人身攻击和违禁内容
    2. 字数控制在 100 字以内
    3. 直接输出结果，不额外解释
    4. 重点关注最近 10 条消息
```

新增同类型变体只需在对应列表下加一项，无需改 Go 代码。`persona` 5 字段全必填，渲染在发给大模型的 user 消息末尾（详见[核心机制](#-核心机制)）。

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
├── start_main.bat / .sh      # 开发启动脚本（含前端构建）
├── build_exe.bat / .sh       # 编译打包脚本（交叉编译 4 平台）
├── version/
│   └── version.go            # 版本号注入（编译时由 -ldflags 写入）
├── apppath/
│   └── apppath.go            # 可执行文件同目录路径解析
├── pool/
│   └── pool.go               # 通用协程池（固定 worker，有界队列）
├── config/
│   ├── config.go             # 运行时配置加载（config.yaml → struct）
│   ├── config_example.yaml   # 内置配置模板（编译时嵌入 exe）
│   ├── prompt_system_example.yaml  # 内置提示词模板（编译时嵌入 exe）
│   ├── embed.go              # //go:embed 模板嵌入
│   ├── init.go               # 首次运行自动创建配置文件
│   └── prompt.go             # 提示词配置加载+热重载+增删改
├── cache/
│   └── cache.go              # 环形缓冲区（零拷贝写入，O(1) 去重）
├── llm/
│   └── llm.go                # 大模型客户端（go-openai SDK，连接池，类型安全）
├── async/
│   └── async.go              # 安全 goroutine 管理器（基于 pool + ctx 自动传播 + panic recover）
├── logutil/
│   └── logger.go             # 日志（zap + lumberjack，20MB 切割，30 天保留）
├── onebot/
│   ├── client.go             # NapCatQQ HTTP API 客户端（resty，自动序列化+重试）
│   └── types.go              # API 数据类型定义
├── bot/
│   ├── polling.go            # 轮询拉取消息 + 去重（context 支持优雅退出）
│   └── handler.go            # 消息处理：白名单 → @检测 → 指令路由
├── cmd/
│   ├── command.go            # Router + 前缀树(trie)路由匹配 + 未匹配兜底 + 安全 goroutine
│   ├── internal_cmd.go        # 内部指令（添加关键字、删除关键字、添加/删除指令规则、帮助）
│   ├── chat_review.go         # chat_review 异步处理函数（缓存窗口 + 组装消息）
│   ├── chat_window.go         # 缓存窗口决策：扩展 vs 重置（纯函数，可单测穷举）
│   └── persona_render.go      # 人格渲染（5 维 → user 消息末尾片段）
└── web/
    ├── server/                # Gin Web 管理面板后端
    │   ├── server.go          # Gin 引擎、API 路由、SPA 回退、优雅关闭
    │   ├── handlers.go        # API 处理函数（login/status/groups/messages）
    │   ├── auth.go            # JWT 签发与校验（HS256，24h 过期）
    │   ├── middleware.go       # 中间件（日志、panic 恢复、CORS、JWT 鉴权）
    │   └── embed.go           # //go:embed static/frontend
    └── frontend/              # uni-app Vue 3 SPA
        └── src/               # 页面：登录、群列表、消息详情
```

### 包依赖关系

```
main → config, llm, logutil, bot, onebot, async, apppath, version, web/server
bot → config, cache, onebot, cmd
cmd → config, cache, llm, onebot, async
web/server → config, logutil, onebot, cache
async → logutil, pool
pool → (无内部依赖：仅标准库 sync)
onebot → (无内部依赖)
cache → (无内部依赖)
llm → (无内部依赖)
config → apppath, logutil
logutil → apppath
apppath → (无内部依赖)
version → (无内部依赖)
```

### 消息处理流程

```
Polling loop (bot/polling.go)
  → 从 NapCat 拉取历史消息 (onebot, resty)
  → 去重：O(1) msgID 集合
  → ProcessMessage (bot/handler.go)
     → 白名单检查
     → 截断到最大长度
     → 存入环形缓冲区（零拷贝写入）
     → @机器人检测（QQ号 + 昵称）
     → 前缀树匹配 → 命中指令：调用该指令 handler（带该指令人格）
     → 未命中：兜底直接发大模型回答（不带人格）
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

**2. 在 `cmd/` 下新建 handler 文件**（如 `weather.go`），handler 为 Router 的方法，签名固定为 `(event, groupID, systemPrompt, keywordPrompt, mentionerNick, extra, persona)`：

```go
func (r *Router) weatherHandler(event onebot.Event, groupID, systemPrompt, keywordPrompt, mentionerNick, extra, persona string) {
    r.Go(func(ctx context.Context) error {
        // 异步安全启动：ctx 自动继承 shutdown 信号（Ctrl+C 可取消进行中的 LLM 调用）
        // systemPrompt  = 机器人身份（QQ+昵称）+ 该指令共享规则
        // keywordPrompt = 该指令的 prompt；persona = 5 维人格渲染（未配则空串）
        // 组装 user 消息：聊天记录 + @者信息 + 关键词 prompt + 人格
        userMsg := buildUserMsg(chatLog, mentionerNick, keywordPrompt, extra, persona)
        reply, err := r.llmClient.Review(ctx, userMsg, systemPrompt)
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
- 编译为单二进制文件（前端页面一并嵌入），丢上去就跑
- **首次启动自动创建配置文件**：exe 运行时若同目录没有 `config.yaml` 和 `prompt_system.yaml`，自动从内置模板生成，编辑后重新运行即可
- 推荐使用 `systemd`（Linux）或任务计划程序（Windows）设为开机自启

## 🧪 测试

三层测试体系：

1. **单元测试**：`cmd/chat_window_test.go` 表格驱动穷举缓存窗口决策（扩展 vs 重置）各分支；决策函数为纯函数、显式传参、无全局依赖。
2. **测试模式基建**：`GOOD_REVIEW_TEST=1` 启动时用 FakeLLM（固定回复并记录每次调用）替代真实大模型、NapCat 指向死地址，并注册 `/api/debug/*` 自测接口（inject/reset/state/trigger）——仅测试模式可达，生产模式会被 SPA fallback 回成页面 HTML。
3. **E2E**：Playwright **API 层自测**（`request` fixture，不启动浏览器），直打真实测试二进制的 HTTP 接口：

```bash
cd tests/e2e && pnpm test
```

- 覆盖：登录鉴权、群数据、消息数据、bot 全流程（注入→触发锐评→断言回复）、缓存命中成本（扩展 vs 重置窗口）。
- 进程内缓存/锚点是全局的，用例串行（`workers: 1`），靠 `/api/debug/reset` 隔离；`trigger` 同步等待 handler 收尾（锚点更新）再返回，异步副作用不跨用例泄漏。
- 详情见 [docs/testing.md](docs/testing.md)。

## 🛠 核心机制

本项目围绕「并发安全 + 优雅退出 + 可测试性」设计了以下机制：

### 异步与并发控制

- **协程池**（`pool/`）：固定 worker（默认 `runtime.NumCPU()*2`）+ 有界任务队列；`Submit` 非阻塞，队列满返回 `false` 由上层处理背压；`Shutdown` 优雅排空。纯标准库，无内部依赖。
- **安全 goroutine 管理器**（`async/`）：`async.Group` 封装协程池，**context 自动继承**（Ctrl+C 可取消进行中的 LLM 调用）、**panic recover** 防止单任务 panic 拖垮进程；`Wait()` 先取消再排空。
- **单写者架构**：轮询是唯一写缓存的 goroutine，配合 `RWMutex`，写侧几乎无竞争。

### 优雅停机

`main.go` 用 `signal.NotifyContext` 捕获 SIGINT/SIGTERM，三层消费同一个 `shutdownCtx`：

```
信号 → shutdownCtx → 轮询循环 select ctx.Done() 正常退出
                    → webSrv.Shutdown(ctx)（10s 超时排空请求）
                    → router.Wait() 等所有 in-flight goroutine（LLM 调用可被取消）
```

### 缓存窗口决策：扩展 vs 重置（成本优化）

每次发大模型前按 token 成本二选一（`cmd/chat_window.go`）：

- **扩展窗口**（缓存命中）：锚点 `LLMAnchor{Start, LastSent}` 定位上次发送窗口，成本 = `命中前缀 × cache_hit_cost + 新增 × cache_miss_cost`；
- **重置窗口**：最近 `llm_send_count` 条，成本 = `全部 × cache_miss_cost`。

三者全满足才扩展（锚点可用 + 扩展更便宜 + 未超 `max_context_tokens` 护栏），否则重置；重置窗口超护栏时从旧到新逐条截断。token 用启发式估算（`cache/token.go`：CJK 每字 1 token，ASCII 每 4 字符 1 token），不建字符串、不分配。

### 无状态请求 + 前缀缓存命中

- **无状态请求**：每次调用大模型（`llm.Review`）都是全新的 `system + user` 单次请求，不维护任何服务端会话。"对话连续感"由机器人把该群聊天记录拼进 user 消息实现——请求无状态，但前缀连续。
- **前缀缓存命中**：DeepSeek 对"从第 0 个 token 完全一致"的请求前缀自动做磁盘缓存（内容寻址、每用户独立、best-effort），命中部分按 `cache_hit_cost` 计费并复用计算。扩展窗口时本次请求头部与上次完全一致 → 命中前面一大段，只对新增消息计未命中。
- **按群隔离**：环形缓存与扩展锚点都按 `groupID` 键控，每个群独立维护聊天记录与窗口——不同群前缀各异、各占各的缓存单元，互不挤占（内容寻址，非"一条缓存被覆盖"）。
- **不破坏命中的关键**：人格渲染放 user 消息**末尾**（见下），让 `system + 以下是群聊记录 + 聊天记录前缀` 在换指令/换人格时保持稳定；只有单群自己前缀变化（被迫重置、长期沉默缓存被清）才会丢命中。

### 人格系统（Persona）

每条指令可配 5 维人格（`config/prompt.go` 的 `Persona`），字段全必填：

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `identity` | 文本 | 身份背景（名字/年龄/职业/经历，具体可信） |
| `personality` | JSON 数组 | 性格特质（行为化描述） |
| `relationship` | JSON 对象 | 与群友的关系（角色 / 对待方式） |
| `system_prompt` | 文本 | 人格级系统指令（行为边界 / 输出要求） |
| `emotion` | JSON 对象 | 情绪维度（维度 → 取值，人格核心） |

**渲染位置**：`RenderPersona`（`cmd/persona_render.go`）把人格渲染成 user 消息**末尾**的片段——不进 system prompt，从而不破坏上方"前缀缓存命中"的稳定前缀。

**内置两条通用规则**（`personaDirective`）：

- **情绪缓和**：用户情绪过于强烈（无论愤怒、悲伤，还是过度兴奋、狂喜）时，情绪反应符合人格但以缓解对方情绪为先，不升级冲突、不火上浇油。
- **表达多样性**：回复要有变化，避免固定口头禅/收尾词，防止自我回音导致的输出趋同。

**无状态机**：不维护人格状态，对话窗口即隐式状态，LLM 是维度变化与回答的最终解释者。

### 数据结构机制

- **环形缓冲缓存**（`cache/`）：定长数组 + 写指针，满了循环覆盖，**零拷贝写入**；`msgIDSet` O(1) 去重；`GetAll()` 两段拼接保持时间序。
- **前缀树指令路由**（`cmd/command.go`）：`trieMatch` 逐字符返回**最长匹配前缀**（「锐评下」优先于「锐评」），O(k) 与路由总数无关；用户指令从 YAML 动态 rebuild，内部指令持久保留。

### 工程健壮性

- **显式依赖注入，零 `init()` 副作用**：所有组件在 `main()` 自上而下构造。
- **防御性**：白名单群、消息按 rune 截断、@检测 QQ号+昵称双校验、配置缺失自动从模板生成并退出。
- **可观测性**：zap + lumberjack 双输出（控制台 + 文件 20MB 轮转、30 天保留、gzip）。
- **Web 面板**：`//go:embed` 内嵌 SPA、JWT(HS256) 鉴权、无免密模式。

---
