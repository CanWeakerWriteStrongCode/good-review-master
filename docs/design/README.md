# 设计文档

这里的每篇回答「这次打算改成什么样」。和 [adr/](../adr/) 成对：

- `design/` 记**拍板前**的提案与依据：测算、对比、被否的版本、验收标准。
- `adr/` 记**拍板后**的结论：选了哪个、代价是什么、为什么否掉别的。

想知道结论，看 ADR 一句话；想知道那句话是怎么来的，回这里翻数字。

## 状态

每篇开头三行：

```
> 状态：Draft / Accepted / Implemented / Abandoned
> 日期：YYYY-MM
> 结论落在：ADR-xxxx（或 domains/ 某篇）
```

| 状态 | 意思 | 怎么办 |
| --- | --- | --- |
| `Draft` | 还在讨论 | 可以推翻，放心改 |
| `Accepted` | 定了，还没动手 | 动手前先看它 |
| `Implemented` | 已落地 | 只有这一类可以考虑删（见下） |
| `Abandoned` | 没做 | 留着——防止半年后有人再提一遍，还得从头算 |

## 关于删

`Implemented` 不等于垃圾。**真正能删的只有纯过程**：

| 可以删 | 不要删 |
| --- | --- |
| 待办清单、实施顺序（commit message 里有） | 测算与实测数字 |
| 逐日的验证记录、中间状态截图 | 被否方案的推演 |
| 事后已经写进 `domains/` 的流程复述 | 「当时为什么不算」的成本对比 |

判据：**删掉之后，下次有人问「为什么不是另一种做法」，还答得上来吗？** 答不上来就留着。

这也决定了链接方向：活性文档只链 `../design/` 这个目录，不链里面的单篇；要删就整篇删，不会留死链。

已经删过两篇：管理指令前移、四处结构债的清理。两篇都是待办与实施顺序，兑现在代码里，没有必要再答一遍「为什么不是另一种做法」。

## 命名

`<主题>-<YYYY-MM>.md`。月份是写的月份，不是计划落地的月份，方便一眼看出哪些已经过期。

## 现有的几篇

| 文件 | 是什么 | 状态 | 结论落在哪 |
| --- | --- | --- | --- |
| [cache-cost-2026-11.md](cache-cost-2026-11.md) | 缓存窗口「扩展」与「重置」的成本测算 | Implemented | [ADR-0006](../adr/0006-token-cost-cache-window.md) |
| [mq-readiness-2026-11.md](mq-readiness-2026-11.md) | 上 MQ 之前必须补什么、什么时候才真该换 | Accepted（本期不做） | [ADR-0005](../adr/0005-orchestration-layer.md) 的「后果」 |
| [orchestrator-workflow-2026-11.md](orchestrator-workflow-2026-11.md) | 域回消息、编排层转发的改造方案：`Pick` / `Signal`、`enter` / `advance`、五条边界条件 | Implemented | [ADR-0005](../adr/0005-orchestration-layer.md)、[message-flow](../architecture/message-flow.md)、[domains/pipeline](../domains/pipeline.md) |
| [command-gate-2026-11.md](command-gate-2026-11.md) | 指令模块前移到 Agent 工作流入口：`#启动` / `#关闭` 门控、会话级开关、`CommandRan` 防止二次执行 | Implemented | 待补 ADR-0007 |
