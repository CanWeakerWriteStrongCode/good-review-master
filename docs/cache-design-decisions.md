# 缓存方案技术决策

## 问题

两次 @bot 查询之间环形缓冲区旋转，导致 chat log 前缀变化，DeepSeek 缓存无法命中。

## 候选方案

### 方案 A：字符串存储

- 存储上次发送的 chat log 字符串 + LastMaxMsgID
- 扩展时拼接字符串：`lastChatLog + BuildChatLog(newMsgsOnly)`
- 缓冲区不变

### 方案 B：大缓冲区 + 窗口锚点（选用）

- 存储上次发送窗口的**首尾两条**消息 MsgID（`Start` 命中前缀起点、`LastSent` 区分新增）
- 扩展时从缓冲区锚点位置读到末尾
- 缓冲上限手动配置 `max_cache_msg`（≈31×`llm_send_count`）
- 每次发送前按**启发式 token 成本**决定扩展还是重置（见 `cache-cost-analysis.md`）

## 对比

| 维度 | 方案 A | 方案 B |
|------|--------|--------|
| 存储 | 字符串 + int64 | 两个 int64（Start + LastSent） |
| 消息格式化 | 可能两次 BuildChatLog | 一次 BuildChatLog |
| 缓冲上限 | 手动配置 | 手动配置（≈31×llm_send_count） |
| 去重 | MsgID 过滤 | 天然无重复 |
| 成本决策 | 拼接 + 计数 | token 估算 + 成本比较 |

## 决策：方案 B（token 成本版）

原因：
1. 更少存储（仅两个 int64）
2. 天然去重（同一段缓冲区）
3. 逻辑更简单（找锚点 → 按 token 成本决定 → 读到尾或重置）
4. 成本按**实际文本 token** 精确计算，长短消息不再一视同仁

## 相比旧条数模型的演进

- 旧：盈亏平衡按条数 `maxExtendNewCount = W×(1-H/M)`，缓冲自动推导 `W + maxExtendNewCount`，重置靠缓冲淘汰隐式触发。
- 新：成本按 token 精确比较（`扩展成本 < 重置成本` 且不超 `max_context_tokens` 护栏），锚点丢失/成本不优才重置；缓冲上限 `max_cache_msg` 手动配置，只作内存与锚点存活边界。
