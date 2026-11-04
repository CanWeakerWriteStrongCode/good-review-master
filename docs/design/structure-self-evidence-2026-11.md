# 让结构自己说话：四处结构债的清理

> 状态：Implemented
> 日期：2026-11
> 结论落在：本文（一次性清理，结论直接进代码）

## 判据

把注释和文档全拿掉，只看类型、字段名、函数签名，能不能还原出这套架构？能，就是自证。

按这个标准量了一遍，下面五件事已经自证：

1. `bot.NewBot` 里八行说完整个拓扑，没有第九种写法。
2. `DomainHandler` 的签名里没有编排层，也没有 `Buffer`——域想直接投给下游，手上没这东西。
3. `exit` 只有 `orchestrator` 有，域汇报「发生了什么」，不知道也不决定「还有没有下一站」。
4. `Item` 自带 `Tag`，`consume` 和 `dispatch` 直接用，全项目没有一处「这条消息属于哪条工作流？」的反查。
5. 六类装配错误（入口未挂载、去向未挂载、重复连线、连线与终点冲突、工作流重名、域重名）全在构造期 panic。

剩下四处不自证。都不难，但都正好落在「读代码时最会先撞上」的位置。

## 一、包注释里的 `edges`

`pipeline/contracts.go` 的包注释写着：

```
本包不知道这个项目有几个域、它们之间怎么接——那是编排层的事（`bot` 的
`nodes` / `edges`）。这里只认"节点名"这个字符串。
```

`edges` 在 topic+tag 改造里已经换成了 `workflows`，只剩这一处没跟上。

包注释是新人翻代码的第一眼，它描述的结构在仓库里查不到——这是最伤自证的一处，也是最便宜的一处。

**改法**：`edges` → `workflows`。

## 二、`forward` 一个参数两种含义

```go
func (o *orchestrator) forward(tag, topic string) (string, string) {
	if tag == "" {
		wf, ok := o.workflows[topic]   // topic 是工作流名
		return wf.entry, topic
	}
	wf, ok := o.workflows[tag]
	return wf.next[topic], tag          // topic 是去向名
}
```

同一个 `topic` 参数，一支装工作流名、一支装去向名。这条规则现在只写在注释里——**结构本身没说**。

**改法**：拆成两个函数，名字就是规则。

```go
// enter 前置选了哪条工作流：返回它的入口域。查不到返回空。
func (o *orchestrator) enter(workflowName string) string

// advance 工作流里的域报了哪类去向：返回下一个域。没连线返回空。
// 终点不在这里判——终点是「明确到此为止」，和「没连线」不是一回事。
func (o *orchestrator) advance(tag, topic string) string
```

`dispatch` 按 `tag` 空/非空分两支，各自调一个名字说明用途的函数；投递那一段抽成 `send`。

**为什么不给 `Route` 加字段**：`Route` 是域的唯一出口。加一个「这个值其实是工作流名」的布尔字段，等于把一个类别的错误做成数据——类型系统在这里帮不上忙，只是把注释挪进了结构体。拆函数更小，测试面也不变。

## 三、`Stage` 一词两义

```go
const (
	StageSync    = "sync"     // 值是节点名
)

type node struct {
	name   string          // 取值是 StageAssets
	stage  *pipeline.Stage // 类型是 Stage
}
```

`pipeline.Stage` 是消费者对象，`pipeline.StageAssets` 是**节点名**，`Item.Stage` 装的还是节点名。同一个词，三个意思，还相邻两行。

**改法**：节点名统一叫 `Node`。

```go
const (
	NodeSync    = "sync"
	NodeAssets  = "assets"
	NodeAgent   = "agent"
	NodeCommand = "command"
)

type Item struct {
	Node string // 目标节点：接 MQ 时它就是 topic（谁消费）
	Tag  string // 属于哪条工作流：接 MQ 时它就是 tag
	...
}
```

`pipeline.Stage`、`StageConfig`、`StageOptions` 不动——它们确实是「协程池」，和节点名不是一回事，改完就不打架了。

**为什么不改成 `Item.Topic`**：`bot.Route.Topic` 装的是**去向事件名**（`tagged` / `replied`），而 MQ 的 topic 是目标地址（`agent`），两者方向相反。`Item.Topic` 会让同一个词在同一行里表示相反的意思：

```go
pipeline.NewItem(name, next, ...)   // name 是目标域，Route.Topic 是去向
```

比现在更糟。等 `Route.Topic` 改名之后，`Item` 再考虑跟 MQ 对齐。

常量值不变（`"sync"` 等），面板指标 `dead_letter_sync` 不受影响。

## 四、`backlog` 对前置静默返回 0

```go
func (o *orchestrator) backlog(name, key string) int {
	if n, ok := o.nodes[name]; ok {
		...
	}
	return 0   // 传前置的名字也走这里
}
```

前置不在 `nodes` 里，所以 `backlog(NodeSync, key)` 返回 0——和「没有积压」不可区分。现在只有 `handleAssets` 查 `NodeAgent`，没踩到；但这是结构留下的坑。

**改法**：加 `lookup(name)`，先查 `nodes` 再比 `router`。

## 一处不改：`next` + `ends` 两个 map

`workflow` 用一个 `map[string]target` 可以让两个互斥检查消失（`target{node: ""}` 即终点）。但那是拿隐式换显式：读代码时要多想一步「空串代表结束」。

`next` 和 `ends` 两个字段名本身就是它俩是什么的说明书，两条检查是这个便宜的价格。**不换。**

## 验收

1. Go 代码里 `grep edges` 为空
2. `forward` 不存在；`enter` / `advance` / `send` 各自只做一件事
3. `grep "StageSync\|Item\.Stage"` 为空；`pipeline.Stage` 只剩类型用法
4. `backlog` 对四个节点名都返回真实积压
5. `go build ./... && go vet ./... && go test ./...` 全绿

## 不做的

- `Route.Topic` 改名——它装的是去向事件名，和 MQ 的 topic 不是一回事。真要改，得连 `Workflow*` / `Topic*` 两组常量一起重新命名，单独议。
- 把 `Route` 拆成「前置的选择」和「域的回执」两个类型——`DomainHandler` 统一返回 `Outcome` 是有意的，拆分会让前置和域各有一套接口。
