# DeepSeek 缓存成本分析（token 精确版）

## 背景

原算法按**消息条数**建模（盈亏平衡 `maxExtendNewCount = W×(1-H/M)`），每条消息无论长短都算 1 单位，长消息群的成本被严重低估。改为按**实际文本的启发式 token 数**计算成本。

## 计量单位：启发式 token 估算

token 数由本地启发式规则估算（`EstimateTokens`），不调用模型 tokenizer，零依赖、速度快：

1. **中文/全角字符**（rune > 0x2E7F，如汉字、全角标点 `：、。！`）→ 每字符计 1 token
2. **其余字符**（英文、数字、半角标点、空格、换行）→ 每 4 字符计 1 token，不足 4 个按 1 计

逐个示例：

| 内容 | 逐个拆 | 估算 token |
|------|--------|-----------|
| `哈哈哈` | 3 个汉字 | 3 |
| `abc` | 3 个英文字母（4 个以内） | 1 |
| `abcde` | 5 个英文字母，`(5+3)/4` | 2 |
| `666` | 3 个数字 | 1 |
| 一条消息 `{"user":"猫娘","content":"今天好开心"}` | JSON 外壳 7 + 猫娘=2 + 今天好开心=5 | 14 |

换算成直观量级：**纯中文 1 token ≈ 1 个汉字**，按平均消息 ~20 字算，**`max_context_tokens` 的 50000 ≈ 约 2500 条消息**的聊天记录。

`ChatLogTokens(msgs)` 按 `BuildChatLog` 的 JSON 行格式（每条 `{"user":"昵称","content":"内容"}`）统计一组消息总 token，跳过空内容，不分配字符串。

为什么用估算而不是真实 tokenizer：成本比较只依赖命中/新增/重置三者的**相对比例**，统一口径即可保证比例准确；护栏 `max_context_tokens` 是留了安全余量的粗略上限，无需精确到个位。

## 成本模型

设：W = `llm_send_count`（重置窗口条数），H = `cache_hit_cost`（默认 0.033），M = `cache_miss_cost`（默认 1.0），P = systemPrompt + 常量前缀「以下是群聊记录：\n」。

每次成功发送后记录锚点：`anchor.Start`（上次窗口首条 MsgID）、`anchor.LastSent`（上次窗口末条 MsgID）。

```
重置成本 = (tokens(P) + tokens(最近 W 条)) × M
扩展成本 = (tokens(P) + tokens(Start..LastSent)) × H   ← 命中前缀
         + tokens(LastSent+1..当前)                × M   ← 新增
```

**决策**：`扩展成本 < 重置成本` 且 `命中前缀+新增 ≤ max_context_tokens` → 扩展（发送 `Start..当前`，锚点不动）；否则重置（发送最近 W 条，锚点移到该窗口首条）。

说明：
- 命中前缀 = 上次请求的完整内容（P + Start..LastSent），因为 provider 按前缀缓存。
- userMsg 常量后缀（「当前@你的是…」+ keywordPrompt）扩展/重置都按 M 计费，互相抵消，不参与比较。
- 若 `anchor.Start` 已不在环形缓冲中（上次最早 msgid 被覆盖），直接走重置。

## 平均消息长度 与 max_cache_msg

中文群聊消息高度偏短（多数 2-30 字符），均值约 15-25 字符，设计取 ~20 字符 ≈ ~20 token/条。

盈亏平衡的扩展窗口（条数）≈ W × (M/H) ≈ **30×W**（与消息长度无关）：

```
重置窗口 token = W × avgTok
扩展窗口增长到 hitT ≈ 重置token/(H/M) 时盈亏平衡
→ 条数 = W×avgTok / (H/M) / avgTok = W/(H/M) ≈ 30×W
```

`max_cache_msg` ≈ W + 30×W = **31×W**：

| W | max_cache_msg | 环形数组内存/群 |
|---|--------------|----------------|
| 30 | ~1000 | ~96KB |
| 200 | ~6200 | ~0.6MB |

数组 96B/条，几千条也就几百 KB，内容受 `max_msg_rune`(500) 截断，不会爆内存。取更小值 = 锚点偶尔被覆盖（缓存红利略减），内存更省。

## 示例（W=30, avgTok=20, H=0.033, M=1.0, P≈30）

首次锐评（无锚点）→ 重置，发最近 30 条（600 token），锚点 = {Start:#1, LastSent:#30}。

| 轮次 | 命中前缀 token (P+Start..LastSent) | 新增 token | 扩展成本 | 重置成本 | 决策 |
|------|------|------|---------|---------|------|
| 首次 | — | — | — | 630 | 重置 |
| +10 条(200 tok) | 630 | 200 | 220.8 | 630 | 扩展，省 65% |
| 再 +50 条(1000 tok) | 830 | 1000 | 1027.4 | 630 | 重置 |

第二轮：扩展成本 220.8 < 重置 630 → 扩展，命中前缀 630 token 只付 3.3%，省 ~65%。第三轮：扩展窗口已长到 40 条，新增 1000 token 使扩展成本 1027 > 重置 630 → 重置，重新从最近 30 条开始。

## 配置

```yaml
runtime:
  llm_send_count: 200      # 重置窗口条数
  max_cache_msg: 6200      # 环形缓冲条数上限（手动，≈31×llm_send_count）
  max_msg_rune: 500        # 单条消息截断
llm:
  cache_hit_cost: 0.033
  cache_miss_cost: 1.0
  max_context_tokens: 50000   # 扩展窗口 token 上限，超限强制重置（防超模型上下文）
```

不再自动推导 `maxExtendNewCount` / `max_cache_msg`；运行时决策完全按上面的 token 成本比较，扩展窗口受 `max_context_tokens` 护栏约束。
