# DeepSeek 缓存成本分析

## 定价模型

| 类型 | 相对价格 |
|------|---------|
| 缓存未命中 | `cache_miss_cost`（配置，默认 1.0） |
| 缓存命中 | `cache_hit_cost`（配置，默认 0.02） |

缓存命中 token 仅需原价的 ~2%。

## 盈亏平衡公式

设：W = `llm_send_count`（默认每次发送条数），H = `cache_hit_cost`，M = `cache_miss_cost`。

```
方案"重置"（发新的 W 条，0 命中）:
  cost = W × M

方案"扩展"（旧 W 条命中 + K 条新消息未命中）:
  cost = W × H + K × M
```

盈亏平衡：

```
W × H + K × M = W × M
K = W × (1 - H/M)
```

## 示例（W=20, H=0.02, M=1.0）

```
K = 20 × (1 - 0.02/1.0) = 19.6 → floor 19
```

**maxExtendNewCount = 19**，即新增 < 19 条时扩展比重置便宜。

| 新增消息 K | 扩展成本 | 重置成本 | 节省 |
|-----------|---------|---------|------|
| 3 条 | 3.4 | 20 | 83% |
| 5 条 | 5.4 | 20 | 73% |
| 10 条 | 10.4 | 20 | 48% |
| 19 条 | 19.4 | 20 | 3% |
| 20 条 | 20.4 | 20 | 超过，应重置 |

## 缓冲区推导

```
max_cache_msg = llmSendCount + maxExtendNewCount
             = 20 + 19 = 39
```

锚点（窗口第一条消息）在缓冲区中最多存活 39 条消息后被淘汰，与成本阈值天然对齐。

## 配置

```yaml
runtime:
  llm_send_count: 20
  cache_hit_cost: 0.02
  cache_miss_cost: 1.0
```

程序启动时自动计算 `maxExtendNewCount` 和 `max_cache_msg`，无需手动配置。
