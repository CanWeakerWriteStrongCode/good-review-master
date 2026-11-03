# 模型调用

`llm` 只管一件事：把一段对话安全、便宜地送出去，拿回文本或工具调用。它不知道什么是群、什么是人格、这一轮该带几条消息——那些在 `speech`。

## 边界

| 它做 | 它不做 |
| --- | --- |
| 拼请求：多模态内容、工具定义、`response_format` | 选窗、拼系统提示、拼聊天记录 |
| 出站限速、熔断、重试 | 决定回复发到哪 |
| 结构化输出的档位降级与记忆 | 保存对话历史（那是 `state` 和 `conversation.Store`） |
| 用量、成本、耗时、拒绝原因的指标 | 解析业务语义 |

## 四个方法

| 方法 | 用在哪 | 特点 |
| --- | --- | --- |
| `SingleChat` | 内部指令生成提示词、生成规则 | 单轮、无工具 |
| `MutiChatWithTool` | 群聊回复、裁判 | 多轮；`tools` 非空时启用原生 function calling，返回的 `ToolCalls` 非空表示"要调工具"而不是最终答案 |
| `ChatJSON` | 需要结构化结果的地方 | 单轮、无工具，按 JSON Schema 约束 |
| `ChatStructured` | 回复排版、主动插话裁判 | 实现与 `ChatJSON` 相同，单独成方法是为了在调用方、测试替身和日志上能区分 |

拼装上有两个细节值得记住：

- `Message.ImageDataURLs` 非空时才改发多模态内容；为空则保持纯文本，**字节与旧路径完全一致**——前缀缓存靠的就是这段字节不变。
- `Tool.ParamsJSON` 是 `json.RawMessage`，直接透传成 OpenAI 的 `function.parameters`，避免被当成字符串二次转义。

## 窗口：扩展还是重置

无状态请求 + 前缀缓存。每次发送时把上次发送的窗口原样带上，再追加新消息，这样服务端能命中前缀；命中价通常只有未命中的几十分之一。

但带太多会撞上下文上限，带太少前缀又白建。所以每次在两种做法里挑一个：

| 做法 | 成本 | 什么时候选 |
| --- | --- | --- |
| 扩展（extend） | 旧前缀按命中价 + 新增按未命中价 | 前缀还热、总 token 没超护栏 |
| 重置（reset） | 新窗口全部按未命中价 | 锚点过期、或扩展后超 `max_context_tokens` |

三个输入：

| 参数 | 默认 | 作用 |
| --- | --- | --- |
| `cache_hit_cost` / `cache_miss_cost` | 0.033 / 1.0 | 相对单价，用来算扩展到底省不省 |
| `max_context_tokens` | 12000 | 扩展后的总上下文上限；只约束 system + 工具清单 + 聊天记录，不管工具结果和回传图片 |
| `cache_ttl_sec` | 900 | 锚点超过这么久没更新就当没有锚点——服务端的 KV 缓存多半已被挤掉，再按命中价算就是自欺 |

`cache_ttl_sec` 故意取偏小：把还在的缓存当成没了，代价只是多发 20 条；反过来则是几万 token 全价。选窗结果会打指标 `llm_window_mode_total{mode=extend|reset}`，命中量在 `llm_cache_hit_total`。

## 限速与熔断

| 机制 | 参数 | 行为 |
| --- | --- | --- |
| 令牌桶限速 | `rate_limit_per_sec` / `rate_limit_burst` | 超了**直接拒绝**，不排队等待——等待只会把协程池的 worker 占住 |
| 熔断 | `breaker_failures`（5） / `breaker_cooldown_sec`（30） | 连续失败到阈值后立刻失败，冷却后进入半开、放一个请求试探 |

被拒的调用记 `llm_rejected_total{reason}`，群里的表现是一句固定话术，不会干等 120 秒。

## 结构化输出

`ChatJSON` 从高到低试三个档位：

```
json_schema（strict） → json_object → 不传 response_format（纯提示词约束）
```

第一个档位失败就降一档，**降到哪里会被记住**，后续请求直接从那一档开始，不再每次都撞一次。当前档位看指标 `llm_response_format_total{mode}`。

## 失败怎么办

| 失败 | 行为 |
| --- | --- |
| 网络错误 / 5xx | 按重试策略重试；连续失败累计到熔断阈值 |
| 超时 | 由 `llm_timeout_sec` 控制（默认 300s），主动插话的裁判另有更短的超时 |
| 限速拒绝 | 立刻返回错误，调用方降级 |
| 熔断打开 | 立刻返回错误，冷却后半开试探 |
| 结构化输出三次降级都失败 | 返回错误，调用方按"没有结构化结果"处理 |
| 上游不支持某个 `response_format` | 降档并记住，不影响本次结果 |

## 相关决策

- 窗口为什么按成本算而不是按条数：[ADR-0006](../adr/0006-token-cost-cache-window.md)

## 代码位置

| 文件 | 看什么 |
| --- | --- |
| `llm/llm.go` | `Client` 接口、`Message` / `Tool` / `ToolCall`、请求组装、指标埋点 |
| `llm/resilient.go` | 限速、熔断、重试的包装顺序 |
| `llm/chat_json_test.go` | 三档 `response_format` 的降级行为 |
| `conversation/token.go` | token 估算（中文按 1 字 ≈ 1 token 粗算） |
| `conversation/cache.go` | 窗口缓存与锚点的读写 |
| `speech/generate.go` | 调用方：怎么决定带哪些消息 |
