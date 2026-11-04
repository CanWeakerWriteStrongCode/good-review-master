# 2026-11 编排层：域回消息，编排层转发

> 状态：Implemented
> 日期：2026-11
> 结论落在：[ADR-0005](../adr/0005-orchestration-layer.md)、[message-flow](../architecture/message-flow.md)、[domains/pipeline](../domains/pipeline.md)

一次改造方案，不是决策记录。要回答的是：怎么让「域」和「顺序」真正分开，以及这样做的边界在哪。

## 一句话

域执行完不自己找下游，而是把结果交回编排层；编排层按一张连线表决定下一个域是谁。这就是「编排层发消息 → 域执行 → 域回消息 → 编排层再发给下一个域」的闭环。

## 判据：结构自证

把注释和文档全拿掉，只看类型、字段名、函数签名，能不能还原出这套架构？能，就是自证。下面这些现在靠结构站住，不靠注释：

1. `DomainHandler` 的签名里没有编排层，也没有 `Buffer`——域想直接投给下游，手上没这东西。
2. `Pick` 和 `Signal` 是两个类型：前置只能回 `Pick`（这条消息进哪条工作流），域只能回 `Signal`（这一步发生了什么）。「一条回执说的是什么」不再靠「tag 空不空」去猜。
3. `exit` 只有 `orchestrator` 有。域汇报事实，不知道也不决定「还有没有下一站」。
4. `Item` 自带 `Tag`，`consume` 和 `dispatch` 直接用，全项目没有一处「这条消息属于哪条工作流？」的反查。
5. 一个词一个意思：消费者对象是 `pipeline.Consumer`，节点名常量是 `pipeline.NodeSync`，消息上的字段是 `Item.Node`。
6. 六类装配错误（入口未挂载、去向未挂载、重复连线、连线与终点冲突、工作流重名、域重名）全在构造期 panic。

## 为什么要这一层

- **ADR-0005 要求域不知道下游是谁**，但改造前域里写死了下游的名字（`b.forward(ctx, b.b2, …)`）。顺序长在代码里，换不了。
- **失败语义不一致**：一处转发失败只记死信，另一处返回 `FailedItem` 让整批重试——重试会把视觉模型再调一遍，白花钱。
- **`Item` 的节点字段语义是歪的**：入口造出来的 item 记的是生产者，消费它的却是另一个域；而告警分类 `dead_letter_<node>` 和幂等键都按这个字段算。
- **加一个域要动八处**：节点常量、配置字段与解析、装配、域实现、起停遍历、测试。

两件**不是**理由的事，先说清楚：不是为了上 MQ（见 [mq-readiness-2026-11.md](mq-readiness-2026-11.md)），也不是为了热部署（拓扑热替换是以后的畅想，本期只做到「拓扑是一张表」）。

## 契约

```go
// Pick 是前置交回的一条选择：这条消息进哪条工作流。
type Pick struct {
    Workflow string // WorkflowChat / WorkflowCommand
    Key      string // workspaceID|chatID，顺序键
    MsgID    string // 幂等键的后半段
    Payload  any    // 可序列化值对象
}

// Signal 是一条事件：域如实汇报这一步发生了什么。它不含去向。
type Signal struct {
    Event   string // EventTagged / EventReplied / EventCommandDone
    Key     string
    MsgID   string
    Payload any
}

type RouterOutcome struct { Failed []pipeline.FailedItem; Picks   []Pick }
type Outcome       struct { Failed []pipeline.FailedItem; Signals []Signal }

type RouterHandler func(ctx context.Context, key string, items []pipeline.Item) RouterOutcome
type DomainHandler func(ctx context.Context, key string, items []pipeline.Item) Outcome
```

编排层自己持有两样东西：

```go
router    *node                // 前置：所有消息共用，不属于任何工作流
nodes     map[string]*node     // 域（接 MQ 时是 topic）
workflows map[string]*workflow // {entry, next, ends}（接 MQ 时是 tag）
```

`pipeline.Consumer` 只管「怎么把一批消息搬过去」。域交回 `Outcome` 之后，投递发生在 consumer 和域之间的一层薄包装里：

```
Consumer 取一批 → domainBatch: out := handler(...); dispatch(tag, out.Signals); return out.Failed
```

所以 `pipeline` 包依然不知道这个项目有几个域，也依然只管载荷搬运。

## 为什么拆成两个类型

一个类型装两种回执（改造中一度叫 `Route`）时，编排层只能靠「tag 是不是空」区分「前置交回的工作流名」和「域交回的事件名」。加一个「这个值其实是工作流名」的布尔字段更糟：那是把一个类别的错误做成数据，类型系统帮不上忙，只是把注释挪进了结构体。

拆成 `Pick` / `Signal` 之后，两种回执各有一个返回类型，装错在编译期就被拦住；`dispatch` 里的空 tag 分支、`isEnd` 里的空 tag 守卫也随之消失。

代价是前置和域各有一套接口。可控：它们本来就只在 `NewBot` 里各接一次，没有第三个地方需要同时接受两者。

## 三件事各归谁

| 东西 | 归谁 | 依据 |
| --- | --- | --- |
| 去向（`next` / `ends`） | 工作流 | 同一个域挂在两条链上可以一条继续、一条到此为止 |
| 事件名（`Event*`） | 域 | 它只汇报事实，不评价够不够格当最后一站 |
| `tag` | 消息 | 前置那一跳写进去，之后每一跳带着走，去向只在本 tag 的工作流里查 |

事件名不必全局唯一：它只在所属工作流里查，所以两条工作流各有一个 `done` 互不干扰——和 MQ 一样，tag 只在 topic 内唯一。

## 边界条件（必须定死的五条）

1. **回执投递失败不重试本批。** 域交回 `Signal` 时它的副作用已经发生了（入库、打标签、发出回复），重跑一遍等于重复执行。投递失败记死信，让这条消息停在这一跳。
2. **关停期投递失败不报警。** 缓冲已关（`pipeline.ErrClosed`）是预期内的排空结果，只记日志。否则正常重启会刷出一堆假死信。
3. **没连线的事件是错误，不是静默丢弃。** 丢弃的同时打 Error 日志——漏连线是装配错误，不是运行期正常分支。
4. **连线指向未挂载的域，构造期 panic。** 装配错误要在启动时炸，不要等某条消息走到那里。
5. **`backlog` 只许查自己的。** `handleAgent` 判断「这一批后面还有没有积压」用 `orch.backlog(自己的节点, key)`，域不许摸别人的队列。前置也有队列，所以 `lookup` 先查 `nodes` 再比 `router`，不能对前置静默返回 0。

## 明确不做

| 不做 | 原因 |
| --- | --- |
| 拓扑热替换 | 本期只要「拓扑是一张表」；drain-swap、版本号、在途清零都是另一件事 |
| 动态加载域实现 | Go 做不到（plugin 在 Windows 上基本不可用，且不能卸载） |
| 把 `Ack` 用起来 | 只有接 MQ 才有意义，内存实现的 `Take` 即出队 |
| 落盘（outbox） | 见 [mq-readiness-2026-11.md](mq-readiness-2026-11.md) 的「本期结论：不做」 |
| 改 ADR-0005 | 决策没变，只是把「没做到」补上 |
| `next` + `ends` 合成一个 map | 隐式空串换显式两条检查不划算，见 ADR-0005 的「被否的方案」 |

## 验收

1. **加一个域只改两处**：`orch.mount(...)` 一次 + `orch.connect(...)` 一条。
2. **`bot/domains.go` 里搜不到 `Buffer`、`Submit`、别人的节点名**。
3. **`pipeline` 包只保留节点名常量**，不承载「这个项目有几个域、它们怎么接」的知识。
4. **四个行为测试全绿**：三段主链路、图片描述入库、只记录账号不回复、管理指令不进缓存。
5. **告警分类按目标节点**：`dead_letter_sync` / `dead_letter_assets` / `dead_letter_agent`。

## 以后再评估的事

- **热部署**：真出现「第二条链」或「想临时关掉某个域」时再谈。那时要加的是 drain-swap 和拓扑版本号，`nodes` / `workflows` 这两张表本身不用改。
- **接 MQ**：`send` 就是唯一的生产者入口，换成 broker 的 topic 后天然对应。真正挡路的仍是状态外置，不是这一层。
