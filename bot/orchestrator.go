package bot

import (
	"context"
	"errors"
	"fmt"

	"good-review-master/logutil"
	"good-review-master/pipeline"
)

// 回执标签：节点回答“接下来属于哪一类”，编排层回答“这一类由谁接手”。
//
// 标签分两种，都放在 Route.Topic 里：
//   - 工作流名（Workflow*）：只有前置会用。它是“这条消息进哪条工作流”，
//     dispatch 把它翻译成那条工作流的入口域。
//   - 去向（Topic*）：工作流内部用。消息还能往下走还是到此为止，
//     看链有没有把它声明为终点（exit）。
//
// 两者不能重名（connect / workflow 都会检查）：同名就分不清是“进工作流”还是“去下一个域”。
const (
	WorkflowChat    = "chat"    // 聊天：打标签 → 生成
	WorkflowCommand = "command" // 管理指令：只走控制平面

	TopicTagged      = "tagged"       // 打完标签的会话：进入生成
	TopicCommandDone = "command_done" // 管理指令已执行
	TopicReplied     = "replied"      // 回复已发出（含不回、放弃）
)

// Route 是一条回执：节点处理完一条消息后，告诉编排层下一站的类别与载荷。
type Route struct {
	Topic   string
	Key     string
	MsgID   string
	Payload any
}

// Outcome 是一批消息的处理结果：哪些要重试、哪些要交回编排层继续转发。
type Outcome struct {
	Failed []pipeline.FailedItem
	Routes []Route
}

// DomainHandler 是一个节点（前置或业务域）。它不持有下游的任何引用，也不碰缓冲，
// 只把结果交回编排层。
type DomainHandler func(ctx context.Context, key string, items []pipeline.Item) Outcome

// node 是一个节点在编排层里的挂载点：缓冲 + 消费者。
type node struct {
	name   string
	buffer pipeline.Buffer
	stage  *pipeline.Stage
}

// orchestrator 是编排层本身：一个实例，前面一个前置，后面几条工作流。
//
// 它只做四件事：把事件放进前置、把前置/域交回的 Route 翻译成下一站、
// 管理各节点的起停。它不知道补全/打标签/生成各自在干什么——那些都是
// workflows / edges / exits 里的一行。
//
// 层次：
//
//	router          前置：所有消息共用（入口 + 补名），不属于任何一条工作流
//	workflows[名字]  工作流名→入口域
//	edges           topic→下一个域（工作流内部的连线）
//	exits           topic 到此为止
//
// 域与域之间没有引用关系：域执行完把 Outcome 交回来，由这里投递给下一跳。
// 这就是"编排层发消息 → 域执行 → 域回消息 → 编排层再发给下一个域"的闭环。
//
// 链什么时候结束，也是这里说了算：同一个域挂在两条链上，可以在一条链上继续
// 往下走、在另一条链上到此为止，域本身一行代码都不用改。
type orchestrator struct {
	router    *node             // 前置：所有消息共用的入口，不属于任何工作流
	nodes     map[string]*node  // 业务域
	workflows map[string]string // 工作流名 → 入口域名
	edges     map[string]string // topic → node name
	exits     map[string]bool   // topic 到此为止

	onDeadLetter func(item pipeline.Item, err error)
}

func newOrchestrator(onDeadLetter func(item pipeline.Item, err error)) *orchestrator {
	return &orchestrator{
		nodes:        make(map[string]*node),
		workflows:    make(map[string]string),
		edges:        make(map[string]string),
		exits:        make(map[string]bool),
		onDeadLetter: onDeadLetter,
	}
}

// attach 建一个节点（缓冲 + 消费者）。前置和业务域用的是同一套东西，
// 区别只在 dispatch 怎么翻译它们交回来的 Route。
func (o *orchestrator) attach(name string, handler DomainHandler,
	config func() pipeline.StageConfig, perChat, global int) *node {
	n := &node{name: name, buffer: pipeline.NewMemoryBuffer(perChat, global)}
	n.stage = pipeline.NewStage(pipeline.StageOptions{
		Name:   name,
		Buffer: n.buffer,
		Config: config,
		Handler: func(ctx context.Context, key string, items []pipeline.Item) []pipeline.FailedItem {
			out := handler(ctx, key, items)
			o.dispatch(ctx, out.Routes)
			return out.Failed
		},
		OnDeadLetter: o.onDeadLetter,
	})
	return n
}

// mount 挂一个业务域。传进来的 handler 不碰缓冲：Stage 调它、拿到 Outcome 之后，
// 由这里负责投递——域因此不需要、也无法知道下游是谁。
func (o *orchestrator) mount(name string, handler DomainHandler,
	config func() pipeline.StageConfig, perChat, global int) {
	if _, ok := o.nodes[name]; ok {
		panic(fmt.Sprintf("bot: 域 %q 重复挂载", name))
	}
	o.nodes[name] = o.attach(name, handler, config, perChat, global)
}

// mountRouter 挂前置：所有消息共用的入口 + 补名。只能有一个。
//
// 前置不放进 nodes：它不是任何一条工作流的一部分，加工作流不该动它，
// 它也不该出现在工作流的拓扑里。
func (o *orchestrator) mountRouter(name string, handler DomainHandler,
	config func() pipeline.StageConfig, perChat, global int) {
	if o.router != nil {
		panic("bot: 前置只能有一个")
	}
	o.router = o.attach(name, handler, config, perChat, global)
}

// workflow 声明一条工作流：从入口域开始，之后走 edges。
//
// 工作流之间不共享域——想共享就在两条工作流里各写一次（同一个域能挂多条链，
// 只是不能既是入口又靠别人推进）。名字也不能和 topic 重名，否则 dispatch 无从判断。
func (o *orchestrator) workflow(name, entry string) {
	if entry == "" {
		panic(fmt.Sprintf("bot: 工作流 %q 没有入口域", name))
	}
	if _, ok := o.nodes[entry]; !ok {
		panic(fmt.Sprintf("bot: 工作流 %q 的入口域 %q 没有挂载", name, entry))
	}
	if _, ok := o.workflows[name]; ok {
		panic(fmt.Sprintf("bot: 工作流 %q 重复声明", name))
	}
	if node, ok := o.edges[name]; ok {
		panic(fmt.Sprintf("bot: 名字 %q 已经是 topic（连到 %q），不能同时当工作流名", name, node))
	}
	o.workflows[name] = entry
}

// connect 声明"这一类去向由哪个域接手"。连线指向未挂载的域是装配错误，
// 构造期就 panic，别等到线上某条消息走到这里才发现。
func (o *orchestrator) connect(topic, nodeName string) {
	if o.exits[topic] {
		panic(fmt.Sprintf("bot: 去向 %q 已声明为终点，不能再连线", topic))
	}
	if _, ok := o.workflows[topic]; ok {
		panic(fmt.Sprintf("bot: 名字 %q 已经是工作流名，不能同时当 topic", topic))
	}
	if _, ok := o.nodes[nodeName]; !ok {
		panic(fmt.Sprintf("bot: 编排层连线指向未挂载的域 %q（topic=%s）", nodeName, topic))
	}
	o.edges[topic] = nodeName
}

// exit 声明"这一类之后没有下一站"。
//
// 终点必须显式写，不能靠"没连线"推断：漏一条连线是装配错误，要看得见；
// 而"这条链到此为止"是设计。两者一旦混同，漏连线的后果就从报错变成静默少走一步。
// 同一个域挂在两条链上时，终点只需要在链上声明，域不用改。
func (o *orchestrator) exit(topic string) {
	if node, ok := o.edges[topic]; ok {
		panic(fmt.Sprintf("bot: 去向 %q 已连线到 %q，不能再声明为终点", topic, node))
	}
	o.exits[topic] = true
}

// dispatch 把节点交回来的回执分派出去，三种可能：
//
//   - 声明为终点（exits）：到此为止。
//   - 连了线（edges）：交给工作流里的下一个域。
//   - 是工作流名（workflows）：交给那条工作流的入口域。前置就靠这一支路由。
//
// 三者都没有是装配错误，记日志丢弃。
//
// 投递失败不重试本批：走到这一步说明节点的副作用已经发生了（入库、打标签、发出回复），
// 重跑一遍等于重复执行。所以记死信让这条消息停在这一跳，而不是把它塞回重试队列。
// 关停期间缓冲会被关掉，那种情况只记日志不报警——那是预期内的排空结果，
// 报警的话每次正常重启都会刷一堆假死信。
func (o *orchestrator) dispatch(ctx context.Context, routes []Route) {
	for _, route := range routes {
		// 终点的判定先于连线：构造期已经保证三者不会同名，这里只是先收口。
		if o.exits[route.Topic] {
			continue
		}
		name, ok := o.edges[route.Topic]
		if !ok {
			if entry, isWorkflow := o.workflows[route.Topic]; isWorkflow {
				name, ok = entry, true
			}
		}
		if !ok {
			logutil.Error("编排层没有这条去向的连线、也不是工作流名，也没声明为终点，消息已丢弃",
				"topic", route.Topic, "key", route.Key, "msgID", route.MsgID)
			continue
		}
		item, err := pipeline.NewItem(name, route.Key, route.MsgID, route.Payload)
		if err != nil {
			logutil.Error("编排层编码下一跳载荷失败，消息已丢弃",
				"topic", route.Topic, "node", name, "err", err)
			o.deadLetter(item, err)
			continue
		}
		if err := o.nodes[name].buffer.Put(ctx, item); err != nil {
			if errors.Is(err, pipeline.ErrClosed) {
				logutil.Warn("编排层投递时缓冲已关闭，消息停在这一跳",
					"topic", route.Topic, "node", name, "item", item.ID)
				continue
			}
			logutil.Error("编排层投递失败，消息停在这一跳",
				"topic", route.Topic, "node", name, "item", item.ID, "err", err)
			o.deadLetter(item, err)
			continue
		}
		o.nodes[name].stage.Submit(route.Key)
	}
}

func (o *orchestrator) deadLetter(item pipeline.Item, err error) {
	if o.onDeadLetter != nil {
		o.onDeadLetter(item, err)
	}
}

// admit 是入站的唯一入口：把一条事件放进前置，由前置补名后决定进哪条工作流。
func (o *orchestrator) admit(ctx context.Context, key, msgID string, payload any) error {
	if o.router == nil {
		return fmt.Errorf("bot: 编排层没有挂载前置")
	}
	return o.put(ctx, o.router, key, msgID, payload)
}

// start 跳过前置，直接进某条工作流。给“已经在别处补全过”的第二个来源用
// （面板触发、定时任务、Webhook）——它们只需要业务链，不需要再过一遍入站与补名。
func (o *orchestrator) start(ctx context.Context, workflow, key, msgID string, payload any) error {
	entry, ok := o.workflows[workflow]
	if !ok {
		return fmt.Errorf("bot: 编排层没有名为 %q 的工作流", workflow)
	}
	n, ok := o.nodes[entry]
	if !ok {
		return fmt.Errorf("bot: 工作流 %q 的入口域 %q 没有挂载", workflow, entry)
	}
	return o.put(ctx, n, key, msgID, payload)
}

// put 把一条载荷放进指定节点并唤醒它。
func (o *orchestrator) put(ctx context.Context, n *node, key, msgID string, payload any) error {
	item, err := pipeline.NewItem(n.name, key, msgID, payload)
	if err != nil {
		return err
	}
	if err := n.buffer.Put(ctx, item); err != nil {
		return err
	}
	n.stage.Submit(key)
	return nil
}

// backlog 返回某个域当前的积压条数。域用它判断"这一批之后还有没有活"，
// 不需要也不该去摸别人的队列。
//
// 断言不会失败：Stage 构造期就要求 Buffer 实现 Peeker（否则 panic）。
// 这里只依赖"数得清"这一件事，不把 Len 塞回 Buffer。
func (o *orchestrator) backlog(name, key string) int {
	if n, ok := o.nodes[name]; ok {
		if p, ok := n.buffer.(pipeline.Peeker); ok {
			return p.Len(key)
		}
	}
	return 0
}

// allNodes 返回前置与全部业务域：起停、排空、指标都要覆盖它们。
// 前置不在 nodes 里，但它的队列同样要排空、协程同样要等。
func (o *orchestrator) allNodes() []*node {
	out := make([]*node, 0, len(o.nodes)+1)
	if o.router != nil {
		out = append(out, o.router)
	}
	for _, n := range o.nodes {
		out = append(out, n)
	}
	return out
}

func (o *orchestrator) run(ctx context.Context) {
	for _, n := range o.allNodes() {
		n.stage.Run(ctx)
	}
}

func (o *orchestrator) closeBuffers() {
	for _, n := range o.allNodes() {
		if err := n.buffer.Close(); err != nil {
			logutil.Warn("关闭阶段缓冲失败", "node", n.name, "err", err)
		}
	}
}

func (o *orchestrator) closeStages() {
	for _, n := range o.allNodes() {
		n.stage.Close()
	}
}

func (o *orchestrator) wait() {
	for _, n := range o.allNodes() {
		n.stage.Wait()
	}
}

// pending 返回所有节点积压总数（关停排空用）。
func (o *orchestrator) pending() int {
	total := 0
	for _, n := range o.allNodes() {
		total += n.buffer.Total()
	}
	return total
}

// active 返回所有节点正在处理批次的会话数之和。
func (o *orchestrator) active() int {
	total := 0
	for _, n := range o.allNodes() {
		total += n.stage.Active()
	}
	return total
}

// idle 表示没有积压也没有在途，可以安全退出。
func (o *orchestrator) idle() bool {
	return o.pending() == 0 && o.active() == 0
}
