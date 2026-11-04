# 四阶段流水线

一条消息从入站队列出来，经过四个处理阶段走到回复或指令执行。阶段之间的传送带是内存队列，每个阶段自己管批量、并发、重试。

前三个阶段是数据平面（补全数据 → 打图标签 → 生成回复）；第四个阶段是指令的控制平面：管理指令在里面执行，不占大模型并发槽、不进群聊缓存。

## 边界

| 它做 | 它不做 |
| --- | --- |
| 把消息按会话分片、批量派发、失败重试、超限记现场 | 决定消息怎么补名（`workspace.PrepareBatch`） |
| 保证同一个群的消息按到达顺序处理、不同群并行 | 决定回复内容（`speech`） |
| 阶段间只传 `pipeline.Item`（JSON 载荷） | 入站 HTTP、发送动作 |

## 四个队列

| 队列 | 谁放 | 谁取 | 载荷 |
| --- | --- | --- | --- |
| B1 | `bot.AcceptEvent` | Stage 1 | `conversation.Inbound` |
| B2 | Stage 1 | Stage 2 | `conversation.Synced` |
| B3 | Stage 2 | Stage 3 | `conversation.Synced`（多了 `Assets` / `ImageNotes`） |
| bCmd | Stage 1 | Stage Command | `conversation.Synced`（管理指令，只走这一个阶段） |

`Item` 只有五个字段：`ID`（`stage|key|消息ID`，天然幂等键）、`Key`（分区键，现在是 `workspaceID|chatID`）、`Stage`、`Body`（JSON）、`Attempts`。载荷类型定义在 `conversation` 里，`pipeline` 不定义——否则底层包为了用值对象会反过来 import 编排层。

## 四个阶段各干什么

| 阶段 | 入口 | 做什么 | 失败去哪 |
| --- | --- | --- | --- |
| Stage 1 补名入库 | `handleSync` | 按工作空间分组 → `PrepareBatch` 补成员名、补被引用消息 → 写 `messages` → 命中管理指令投 bCmd，其余投 B2 | 重试 `sync_retries` 次，超限进 alerts |
| Stage 2 图片标签 | `handleAssets` | 查图库有没有这条图的描述，没有才调视觉模型 → 写 `image_notes`；没打完的写 `asset_tag_jobs` 欠条 → 投 B3 | 欠条留给后台补，不阻塞消息 |
| Stage 3 生成回复 | `handleAgent` | 进群聊缓存 → 学身份 → 判断该不该回 → 交 `speech` | 生成的失败由 `speech` 自己兜 |
| Stage Command 管理指令 | `handleCommand` | 交 `speech.RunInternalCommand`：再判一次匹配 → 判权限 → 执行 handler | 不重试（`Retries` 固定 0，指令副作用不幂等），失败只记日志 |

Stage 1 的分流用的是只读匹配（`speech.MatchInternalCommand`），它不执行任何东西；真正的执行集中在 `RunInternalCommand`，权限判定只写在它里面。分流与执行各判一次：两次之间路由表可能被别的指令改过（`#删除人格` 会把关键字删掉），第二次失配就丢弃（正文在 Stage 1 已经入库）。

`record_only_accounts` 里的账号不走指令阶段：指令消息按普通消息进 Stage 3，由那里的既有分支回固定话术。

Stage 3 里有两个容易看漏的取舍：

- **只记录不回复的账号**（`record_only_accounts`）到这里就结束：缓存、入库、标签全部照做，只在被点名时回一句固定话术。所以主备切换时备份号的数据一直是热的。
- **主动插话是 latest-wins**：同一批里只看最后一条；如果这个会话的 B3 还有积压，这次干脆不判，等下一批的最后一条。刷屏时不会每条都跑一次裁判。

## 调度方式

```mermaid
flowchart TB
  subgraph 入站
    IN["b1.Put(Inbound)"]
  end
  subgraph "Stage 1 · sync"
    P1["固定协程池 ×sync_concurrency"]
    R1["ready 队列<br/>队首=最久没处理的会话"]
    K1["inflight 标记<br/>同一会话只允许一个协程"]
  end
  subgraph "Stage 2 · assets"
    P2["固定协程池 ×asset_concurrency"]
    K2["同样按会话串行"]
  end
  subgraph "Stage 3 · agent"
    P3["固定协程池 ×agent_concurrency"]
    K3["同样按会话串行"]
  end
  subgraph "Stage Command · 管理指令"
    PC["固定协程池 ×command_concurrency"]
    KC["同样按会话串行"]
  end
  IN --> P1
  R1 --> P1
  P1 --- K1
  P1 -->|"命中管理指令：bCmd.Put + Submit(key)"| PC
  PC --- KC
  P1 -->|"b2.Put + Submit(key)"| P2
  P2 --- K2
  P2 -->|"b3.Put + Submit(key)"| P3
  P3 --- K3
```

要点：

1. **协程不绑定会话**。池里的协程从就绪队列挑一个 key 处理一批，交还后再挑下一个，所以协程数等于 `*_concurrency`，跟有多少个群无关；空闲的群只是队里的一项。
2. **同一个 key 同一时刻只有一个协程持有**（`inflight`），这就是"同一个群的消息按到达顺序处理"的保证；不同 key 并行。
3. **一个协程一次最多连续吃 3 批**同一会话（`maxBurstPerTurn`），吃完让位，防止活跃的群把池子占死。
4. **参数是现读的**（`StageConfig`）。`settings` 表里改了批量/并发/重试，下一批就生效，不用重启。

## 参数

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `sync_batch` / `sync_linger_ms` | 20 / 20 | 一次取几条、凑批最多等多久 |
| `sync_concurrency` / `sync_retries` | 8 / 3 | 库只开一个连接，并发再高也是排队 |
| `asset_batch` / `asset_concurrency` | 4 / 3 | 实际出站请求还要乘 `tag_concurrency` |
| `agent_batch` / `agent_concurrency` | 1 / 3 | 对齐模型服务的并发槽位（`-np`），别越过它 |
| `command_batch` / `command_concurrency` | 1 / 2 | 管理指令阶段；没有 `command_retries`，代码里固定 0 |
| `per_chat_capacity` | 500 | 单群积压上限 |
| `global_capacity` | 5000 | 全进程积压上限 |
| `drain_timeout_sec` | 10 | 关停时最多等多久把存量做完 |

队列满时 `Put` **阻塞**（由 ctx 控制），不静默丢。入站那一层最多等 2 秒，等不到就回 AstrBot 503。

## 关停

```mermaid
sequenceDiagram
  participant A as app
  participant B as bot.Run
  participant W as 四个 Stage
  B->>B: 起 worker（用独立 ctx，不受应用 ctx 影响）
  A->>B: ctx 取消
  B->>W: drain(DrainTimeout)：等 pending 和 active 都归零
  B->>W: closeBuffers + Close + Wait
  B-->>A: 退出，app 接着关库
```

排空失败只记一条 WARN，不阻塞退出。消息正文在 Stage 1 就已经同步入库了，最后几条没跑完最多是"没有回复"，不会"消息丢了"。

## 失败怎么办

| 失败 | 现象 | 处理 |
| --- | --- | --- |
| 载荷解码失败 | 这条永远解不开 | 直接进失败列表，不重试 |
| 工作空间已不存在 | 整批跳过 | 记 DEBUG，不报错 |
| 写库失败 | `SaveMessage` 返回 0 | 不阻塞，这条当"没有行号"继续走 |
| 阶段处理超重试次数 | 面板的"异常现场"里出现 | 写 `alerts`，人工看一眼 |
| 关停排空超时 | 日志 `关停排空超时` | 有在途生成没结束，属正常 |

## 相关决策

- 为什么不上 MQ：[ADR-0005](../adr/0005-three-stage-pipeline.md)
- 写库为什么是先 UPDATE 后 INSERT：[ADR-0004](../adr/0004-update-before-insert.md)

## 代码位置

| 文件 | 看什么 |
| --- | --- |
| `pipeline/contracts.go` | `Item`、`NewItem`、`DecodeItem`，阶段常量 |
| `pipeline/buffer.go` | `Buffer` 接口、`MemoryBuffer` 的阻塞与关闭语义 |
| `pipeline/worker.go` | `Stage` 的池、ready 队列、inflight、`maxBurstPerTurn`、重试与死信 |
| `bot/ingest.go` | `Run`（起停、排空）、`AcceptEvent`（入 B1） |
| `bot/stages.go` | `handleSync` / `handleAssets` / `handleAgent` / `handleCommand` |
| `conversation/types.go` | `Inbound`、`Synced` 的字段与谁填什么 |
