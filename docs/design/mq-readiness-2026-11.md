# 2026-11 上 MQ 之前的账

> 状态：Accepted（本期不做）
> 日期：2026-11
> 结论落在：[ADR-0005](../adr/0005-orchestration-layer.md) 的「后果」

一次评估，不是决策。回答三个问题：这套流水线能不能整体换成 MQ、换之前必须补什么、什么时候才真的该换。

结论会随触发条件变化重写；真要拍板时另开 ADR。

> 2026-11 补充：按这份评估的先做项，已经落了四件不依赖 MQ 的预留——
> `Buffer` 只留「放、取、确认、关、量」，把「看得见队列」的三处能力收进 `Peeker`
> （`Consumer` 两个都要，构造期检查；这是把「传输」和「怎么派活」分开说）；
> `BatchHandler` 写明了一批的边界（攒批与按 Key 分组是适配层的活）、
> `processed_items` 台账（把重投变幂等，本期只建表与接口）、
> 入口分层（`admit` 固定进前置，`start` 可跳过前置直接进某条工作流）。
> 结构随后也理顺了两道：**入口与补名抽成工作流之外的前置**（`mountRouter`），
> 业务拆成两条互不相连的工作流（`workflow` 声明）；**topic + tag** 也对齐了
> RocketMQ—— `Item.Node` 是目标域（就是 topic，谁消费），`Item.Tag` 是所属工作流
> （就是 tag，属于哪条链）。去向只在本工作流的 `next` / `ends` 里解析，两条工作流
> 各有一个 `done` 互不干扰，换 MQ 时一个域对应一个 topic、消费组配置不用复制。
> 设计见 [orchestrator-workflow-2026-11.md](orchestrator-workflow-2026-11.md)。

## 结论

1. **能换**。缓冲是接口、载荷是 JSON、`Item.ID` 是幂等键、`Item.Key` 正好是分区键——口子是设计时留好的。
2. **但真正挡路的不是消息层，是状态层**。去重表、发送台账、人格绑定、群聊缓存全在进程内存里；上 MQ 基本等于上多实例，这些不先外置，换完只会更乱。
3. **不该全换**。同一个进程里的四个节点之间走 MQ，收益为零、延迟变差、还丢掉现有的背压。只有「跨进程的那条边界」值得走 MQ。
4. **有个更便宜的替代**：进程崩溃不丢在途这件事，用 state.db 加一张 outbox 表就能做到，不必先引中间件。
5. **本期两件都不做**：先按内存把编排层拆出来（[orchestrator-workflow-2026-11.md](orchestrator-workflow-2026-11.md)）——既不落盘也不引 MQ。改完再看要不要处理「崩溃丢在途」。

## 已经具备的前提

| 前提 | 现状 | 位置 |
| --- | --- | --- |
| 缓冲是接口 | `Put` / `Take` / `Ack` / `Close` / `Total`；`TakeReady` / `Len` 在 `Peeker` 里，`Consumer` 构造期要求两者都有 | `pipeline/buffer.go` |
| 载荷可序列化 | `Body` 是 JSON，`Inbound` / `Synced` 都是值对象，没有 `*Workspace` 这类指针 | `pipeline/contracts.go`、`conversation/types.go` |
| 幂等键 | `Item.ID = node\|tag\|key\|消息ID` | `pipeline/contracts.go` |
| 幂等台账 | `processed_items` 表 + `Processed` / `MarkProcessed` / `PurgeProcessed`（成功之后记账，崩溃时宁可重做） | `state/processed.go` |
| 分区键 | `Item.Key = workspaceID\|chatID` | `conversation.ChatKey` |
| 批的边界 | `BatchHandler` 的注释写明：非空、同 Key、到达顺序——跨 key 的批必须由适配层先分组 | `pipeline/consumer.go` |
| 域不反向调用下游 | 域只返回 `Outcome`，不持有 `Buffer` | `bot/orchestrator.go` |

这几条决定了「把 `MemoryBuffer` 换成 `KafkaBuffer`」在结构上是成立的。下面是不成立的部分。

## 六个缺口

### 1. `Ack` 是个空壳

**现状**：`Buffer` 接口声明了 `Ack`，但全仓没有一处调用它——内存实现是「`Take` 即出队」，`Consumer` 只在 `FailedItem` 上重试，成功什么都不做。

**MQ 下**：没 ack 的消息会被重投，等于成功处理过的消息再来一遍。

**要改**：`Consumer` 的取批与重试循环补出「成功 → `Ack`」「失败超限 → `Ack` + 死信」两条路径。这是唯一一件不补就不能上 MQ 的事。

### 2. 并发与顺序的语义会变

**现状**：并发 = `*_concurrency` 个协程；顺序 = `inflight` 标记保证同一个 `key` 同一时刻只有一个协程持有；`ready` 队列 + `maxBurstPerTurn` 做公平。

**MQ 下**：并发 = 分区数 × 消费者数；顺序 = 同 `key` 落同一分区。`inflight`、`ready`、`maxBurstPerTurn` 整体退化成消费组的内建行为。

**要改**：`ConsumerConfig.Concurrency` 的含义从「协程数」变成「分区数 / 预取数」，配置项与文档都要重写。好消息是顺序保证更硬。

### 3. `Batch` / `Linger` 会作废

`takeBatch` 的「凑批 20ms、最多 20 条」是消费端行为。MQ 对应 `fetch.min.bytes` / `max.poll.records` / `linger.ms`，属于客户端或 broker 配置，留在代码里没意义。

### 4. 背压消失

**现状**：队列满 `Put` 阻塞，入站最多等 2 秒，等不到回 503——这是「下游处理不过来」的显式信号。

**MQ 下**：写进去就返回，信号变成 lag 指标。AstrBot 那边的重推策略也得跟着改。

**这是行为变化，不是实现细节**：要么在 lag 高时主动拒收，要么接受「入站永不失败、恢复靠消费端追平」。

### 5. `Item.Node` 的语义

它记的是**目标节点**，不是生产者：前置造出「进聊天工作流」那条 item 时，`Node` 已经是 `assets`，不是 `sync`。幂等键与告警分类（`dead_letter_<node>`）都按它算。

接 MQ 时这一条正好对上：`Node` 就是 topic。剩下要改的只是名字（如果以后要按去向再细分），不是语义。

### 6. 状态还在进程里

| 状态 | 现在 | 多实例后会怎样 |
| --- | --- | --- |
| `messageDedup` | 进程内 TTL 表（10 分钟，上限 4096 条） | 同一条消息在两个实例各处理一遍 |
| `SendLedger` | `NewMemoryLedger()`，进程内 | 发送台账各记一份，重试判定失效 |
| `groupPersona` | `Router` 内存 map（源关键字已写库） | 人格切换只在一个实例生效 |
| 群聊缓存与锚点 | `conversation.Store`，锚点有落库通道 | 两个实例各有一份窗口，选窗决策互相打架 |

**所以真正的门槛是状态层**（行 1、2、3、4 都要外置），而不是消息层。这部分工作量比换 Buffer 大得多，而且和 MQ 是可以分开做的两件事。

## 一条硬约束：管理指令不允许重投

管理指令的副作用不幂等——`#添加人格` 重投一次就是两个人格。现在靠 `commandConfig()` 里硬编码 `Retries: 0` 保证，属于「代码里守住」。

换 MQ 时这一条必须落成显式配置：关闭重投，或者让指令节点干脆留在进程内。**会重投的通道不适合承载不幂等的副作用**，这是选型时先要划出来的边界。

## 建议路线

| 步骤 | 做什么 | 为什么是这个顺序 |
| --- | --- | --- |
| ① | 先做内部重构：域改成返回 `Outcome/Signal`、编排层用一张连线表装配。**这是本期唯一要做的**，方案见 [orchestrator-workflow-2026-11.md](orchestrator-workflow-2026-11.md) | 纯内部改动，零行为变化；不做这步，MQ 版会把已处理的消息重投一遍。`Ack` 留到真要接 MQ 时再补，现在补没有消费方 |
| ② | 再把进程内状态外置：`dedup`、`SendLedger` 进 state.db；`groupPersona` 已经有 `personaStore`；缓存接受「重启冷一遍」 | 先判断是否真的需要多实例。不需要就到此为止 |
| ③ | 只把入口那一跳（入站 → 前置）换成 MQ | 这是唯一「外部世界推消息进来」的地方，收益最大：重推、503、重启丢在途，一次全解决 |

关键在第 ③ 步的范围：**内部几条工作流之间的几跳继续留在进程内**。进程内交接是微秒级，过一趟本地 broker 要毫秒级，还要额外背上「ack、位点、再均衡」三套语义——换来的是零收益。

## 更便宜的替代：outbox

如果痛点只是「进程崩溃丢在途」（ADR-0005 已承认的代价），不引 MQ 也能解决：state.db 已经是唯一写入口、消息正文已经入库，加一张表 + 一个扫描器就等价于「持久化队列」。

```sql
outbox(
  item_id     TEXT PRIMARY KEY,  -- 幂等键：node|tag|key|消息ID
  topic       TEXT,              -- 目标节点
  key         TEXT,              -- workspace|chatID，同时是顺序键
  payload     BLOB,              -- JSON
  status      TEXT,              -- pending | done | failed
  attempts    INTEGER,
  created_at  INTEGER,
  updated_at  INTEGER
)
```

扫描器按 `key` 取 `pending`、处理完置 `done`、超次置 `failed`——语义就是「至少一次投递 + 同 key 串行」，和现在的内存队列一致，只是把「在途」落到了盘上。

它和上 MQ 不冲突：将来真要接 MQ，这个扫描器就是现成的生产者。

### 本期结论：不做

先纠正一个容易被当成理由的直觉：**“频繁写库伤硬盘”在当前量级下不成立**。实测 `log/bot.log`，2026-10-30 14:24 到 11-04 19:38 共 1412 条 IM 事件，约 270 条/天；而且库跑在 WAL + `synchronous=NORMAL`（`state/state.go` 的连接串），提交并不 `fsync`，只有 checkpoint 才刷盘。就算把写入放大三倍，一天也就几千次小事务，任何盘都不会成为瓶颈。

所以拒绝它的理由不是性能，而是这四条：

1. **写入放大 + 单写连接**。一条消息现在大约 2~4 次事务（`SaveMessage`、`UpdateMessageImages`、欠条、回复入库、锚点）；加 outbox 变成每段一插一改，约 9 次，且全落在 `MaxOpenConns(1)` 那条串行连接上。补全那一段是**同步入库**（`PrepareBatch` 返回前必须写完），排队延迟会直接变成入站 503 的概率——这是架构耦合，不是 IOPS 问题。
2. **写入形态变差**。大量小事务会让 WAL 反复触发 checkpoint（默认 1000 页 ≈ 4MB）。小事务密堆是最不划算的写法，远不如“攒一批、单事务多行”。
3. **语义污染**。`state.db` 会同时装“事实”（消息、台账、告警）和“在途”（队列位点），面板统计、备份、重建、迁移全都得先区分这两类。
4. **收益为负**。崩溃后把几分钟前的消息重新生成一遍再发出去，体验比“这条没回”更糟；而且重放时群聊窗口与 `LLMAnchor` 已经变了，重放结果和当时不可比。何况正文本来就没丢。

判据一句话：**数据库存“事后要查的”，内存存“正在路上的”。**

什么时候重新评估：出现多实例（这是跨进程问题，不是硬盘问题），或“重启必须补发”变成硬需求。到那时也不是每段写库，而是“入站边界写一次 + 批量扫描 + 单事务多行”。

## 什么时候该上

| 触发条件 | 结论 |
| --- | --- |
| 要跑多实例（HA / 水平扩展） | 该上，且**先做状态外置** |
| 要让面板、CLI、定时任务、另一个 Agent 作为独立进程投递消息 | 该上，这是 MQ 的正经用途 |
| 只想要「消息不丢」 | 上 outbox，别上 MQ |
| 只想要「编排更灵活」 | 不上，`Outcome/Signal` + `workflow` 就够 |
| 内部四个节点之间想「解耦得更彻底」 | 不上，进程内换 MQ 是纯粹的延迟税 |

## 影响面

| 模块 | 要改什么 |
| --- | --- |
| `pipeline.Buffer` | 新增 broker 实现；`Ack` 变成真语义；`Linger` 退役（攒批交给 broker 的批量拉取） |
| `pipeline.Peeker` | 要么给消费端维护本地预取队列并实现它（语义变成“本地还有多少”），要么不套 `Consumer`、按 MQ 自己的消费循环另写一套调度 |
| `pipeline.Consumer` | 并发与顺序交给分区；`inflight` / `ready` / `burst` 要么删要么退化成客户端预取 |
| `bot` | 入站 503 语义变化；关停从「排空 pending」改成「等 lag 归零或超时」 |
| `config` | `*_batch` / `*_linger` 退役，新增分区数、预取数 |
| `telemetry` | `PipelineNodeBacklog` 从 `Buffer.Total()` 换成 broker lag；面板与 Grafana 跟着改 |
| `state` | 若走 outbox：新增表与扫描器；`dedup` / `SendLedger` 若要外置也在这里 |
| ADR-0005 | 「不引消息队列」的前提是单机单进程；多实例一旦成立，那篇要标成「已被取代」 |

## 局限

- 没有压测数据。延迟量级（进程内微秒级 / 本地 broker 毫秒级）是估计，不是测出来的。
- 没比较具体 broker（Kafka / NATS / SQS / Rabbit）的运维成本与顺序能力差异，只讨论了「要不要」。
- 「状态外置」的工作量是估的，没有逐项拆解迁移路径。
- 触发条件里「多实例」的阈值没定义——真要拍板，得先看 `pipeline_node_backlog` 与 `llm` 并发槽的实际水位，而不是凭感觉。
