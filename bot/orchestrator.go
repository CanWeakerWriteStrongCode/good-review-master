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
//     dispatch 把它翻译成那条工作流的入口域，同时把它记成消息的 tag。
//   - 去向（Topic*）：工作流内部用。消息还能往下走还是到此为止，
//     看这条工作流有没有把它声明为终点（exit）。
//
// 去向不必全局唯一：它只在所属的那条工作流里查，所以两条工作流各有一个 “done”
// 互不干扰——和 MQ 一样，tag 只在 topic 内唯一。
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

// workflow 是一条工作流：从入口域开始，之后按去向走。
//
// 它对应的就是 MQ 的一条 tag：消息带着这个 tag 在域之间走，
// 去向表只在本工作流内有效。两条工作流可以各有一个 “done”。
type workflow struct {
	entry string            // 入口域：消息带着这个 tag 第一次出现时的 topic
	next  map[string]string // 去向 → 下一个域
	ends  map[string]bool   // 去向 → 到此为止
}

// orchestrator 是编排层本身：一个实例，前面一个前置，后面几条工作流。
//
// 它只做四件事：把事件放进前置、把前置/域交回的 Route 翻译成下一站、
// 管理各节点的起停。它不知道补全/打标签/生成各自在干什么——那些都是
// workflows 里的一行。
//
// 层次（右边是接 MQ 时的对应）：
//
//	router             前置：所有消息共用（入口 + 补全），不属于任何工作流
//	workflows[名字]     工作流 ←→ tag
//	  .entry           入口域 ←→ 消息的 topic（谁消费）
//	  .next / .ends    工作流内部的去向表
//
// 域与域之间没有引用关系：域执行完把 Outcome 交回来，由这里投递给下一跳。
// 这就是“编排层发消息 → 域执行 → 域回消息 → 编排层再发给下一个域”的闭环。
//
// 链什么时候结束，也是这里说了算：同一个域挂在两条链上，可以在一条链上继续
// 往下走、在另一条链上到此为止，域本身一行代码都不用改。
type orchestrator struct {
	router    *node                // 前置：所有消息共用的入口，不属于任何工作流
	nodes     map[string]*node     // 业务域（topic）
	workflows map[string]*workflow // 工作流名（tag）→ 拓扑

	onDeadLetter func(item pipeline.Item, err error)
}

func newOrchestrator(onDeadLetter func(item pipeline.Item, err error)) *orchestrator {
	return &orchestrator{
		nodes:        make(map[string]*node),
		workflows:    make(map[string]*workflow),
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
			return o.consume(ctx, key, items, handler)
		},
		OnDeadLetter: o.onDeadLetter,
	})
	return n
}

// consume 按 tag 把一批消息分组，各自交给 handler，再按各自的 tag 翻译去向。
//
// 一个域只服务一条工作流时（现在四个域都是）这是零成本：只有一种 tag，
// 快路径只扫一遍就走到原处。一个域被多条工作流复用时，一批里会出现两种以上的
// tag，必须分开处理——和 MQ 消费者订阅多个 tag 的语义一样：先按 tag 分类，
// 再分派给各自的处理逻辑，而不是按第一条算（那会把第二条串到别的工作流的下游）。
func (o *orchestrator) consume(ctx context.Context, key string,
	items []pipeline.Item, handler DomainHandler) []pipeline.FailedItem {
	if len(items) == 0 {
		return nil
	}

	// 快路径：一种 tag，不建 map。
	first := items[0].Tag
	same := true
	for _, it := range items[1:] {
		if it.Tag != first {
			same = false
			break
		}
	}
	if same {
		out := handler(ctx, key, items)
		o.dispatch(ctx, first, out.Routes)
		return out.Failed
	}

	// 慢路径：按 tag 分组。order 让处理顺序确定，出问题好复现。
	groups := make(map[string][]pipeline.Item, 2)
	order := make([]string, 0, 2)
	for _, it := range items {
		if _, ok := groups[it.Tag]; !ok {
			order = append(order, it.Tag)
		}
		groups[it.Tag] = append(groups[it.Tag], it)
	}

	var failed []pipeline.FailedItem
	for _, tag := range order {
		out := handler(ctx, key, groups[tag])
		o.dispatch(ctx, tag, out.Routes)
		failed = append(failed, out.Failed...)
	}
	return failed
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

// workflow 声明一条工作流：从入口域开始，之后按 connect 连的去向走。
//
// 它就是 MQ 的一条 tag。去向表只在它自己内部有效，所以两条工作流可以各有一个
// "done"；同一个域也可以同时挂在两条工作流上，消息带着各自的 tag，互不串线。
func (o *orchestrator) workflow(name, entry string) {
	if name == "" {
		panic("bot: 工作流名不能为空")
	}
	if entry == "" {
		panic(fmt.Sprintf("bot: 工作流 %q 没有入口域", name))
	}
	if _, ok := o.nodes[entry]; !ok {
		panic(fmt.Sprintf("bot: 工作流 %q 的入口域 %q 没有挂载", name, entry))
	}
	if _, ok := o.workflows[name]; ok {
		panic(fmt.Sprintf("bot: 工作流 %q 重复声明", name))
	}
	o.workflows[name] = &workflow{entry: entry, next: map[string]string{}, ends: map[string]bool{}}
}

// connect 声明「工作流 X 的这类去向由哪个域接手」。连线指向未挂载的域是装配错误，
// 构造期就 panic，别等到线上某条消息走到这里才发现。
//
// 同一个去向在同一工作流里重复连到不同域也会 panic：那是两条路径抢同一个 tag，
// 靠后写覆盖先写会让装配看起来成功、运行时分流。
func (o *orchestrator) connect(workflowName, topic, nodeName string) {
	wf, ok := o.workflows[workflowName]
	if !ok {
		panic(fmt.Sprintf("bot: 工作流 %q 还没声明，不能连线（去向=%s）", workflowName, topic))
	}
	if wf.ends[topic] {
		panic(fmt.Sprintf("bot: 工作流 %q 的去向 %q 已声明为终点，不能再连线", workflowName, topic))
	}
	if prev, ok := wf.next[topic]; ok && prev != nodeName {
		panic(fmt.Sprintf("bot: 工作流 %q 的去向 %q 已连到 %q，不能再连到 %q",
			workflowName, topic, prev, nodeName))
	}
	if _, ok := o.nodes[nodeName]; !ok {
		panic(fmt.Sprintf("bot: 连线指向未挂载的域 %q（工作流=%s 去向=%s）", nodeName, workflowName, topic))
	}
	wf.next[topic] = nodeName
}

// exit 声明「工作流 X 的这类去向之后没有下一站」。
//
// 终点必须显式写，不能靠"没连线"推断：漏一条连线是装配错误，要看得见；
// 而"这条链到此为止"是设计。两者一旦混同，漏连线的后果就从报错变成静默少走一步。
func (o *orchestrator) exit(workflowName, topic string) {
	wf, ok := o.workflows[workflowName]
	if !ok {
		panic(fmt.Sprintf("bot: 工作流 %q 还没声明，不能声明终点（去向=%s）", workflowName, topic))
	}
	if node, ok := wf.next[topic]; ok {
		panic(fmt.Sprintf("bot: 工作流 %q 的去向 %q 已连线到 %q，不能再声明为终点",
			workflowName, topic, node))
	}
	wf.ends[topic] = true
}

// dispatch 把节点交回来的回执分派出去。tag 是「当前消息属于哪条工作流」，
// 前置投递时打上，之后每一跳都带着走。
//
// 两类回执走两条路，各调一个名字说明用途的函数：**前置交回的是工作流名**（enter），
// **域交回的是去向**（advance）。tag 为空就是前者——全文件只在这里用空值区分层级。
//
// 投递失败不重试本批：走到这一步说明节点的副作用已经发生了（入库、打标签、发出回复），
// 重跑一遍等于重复执行。所以记死信让这条消息停在这一跳，而不是把它塞回重试队列。
// 关停期间缓冲会被关掉，那种情况只记日志不报警——那是预期内的排空结果，
// 报警的话每次正常重启都会刷一堆假死信。
func (o *orchestrator) dispatch(ctx context.Context, tag string, routes []Route) {
	for _, route := range routes {
		if tag == "" {
			// 前置选了工作流：进它的入口域，工作流名从此成为消息的 tag。
			o.send(ctx, o.enter(route.Topic), route.Topic, route)
			continue
		}
		// 工作流里的域报了去向：声明为终点就结束，连了线就交给下一个域。
		if o.isEnd(tag, route.Topic) {
			continue
		}
		o.send(ctx, o.advance(tag, route.Topic), tag, route)
	}
}

// enter 前置选了哪条工作流：返回它的入口域。查不到返回空。
func (o *orchestrator) enter(workflowName string) string {
	wf, ok := o.workflows[workflowName]
	if !ok {
		return ""
	}
	return wf.entry
}

// advance 工作流里的域报了哪类去向：返回下一个域。
//
// 没连线返回空。终点不在这里判——「明确到此为止」与「漏了连线」是两件事，
// 前者走 isEnd，后者要作为装配错误叫出来。
func (o *orchestrator) advance(tag, topic string) string {
	wf, ok := o.workflows[tag]
	if !ok {
		return ""
	}
	return wf.next[topic]
}

// isEnd 判断这条回执是不是某条工作流声明过的终点。
func (o *orchestrator) isEnd(tag, topic string) bool {
	if tag == "" {
		return false // 前置回的是工作流名，不是任何一条链的终点
	}
	wf, ok := o.workflows[tag]
	return ok && wf.ends[topic]
}

// send 把一条回执投给指定节点。
//
// name 为空说明没查着去哪：前置回了不存在的工作流名，或域回了本工作流没连线的去向。
// 两种都是装配错误，记日志丢弃。
func (o *orchestrator) send(ctx context.Context, name, tag string, route Route) {
	if name == "" {
		logutil.Error("编排层不知道这条回执去哪，消息已丢弃",
			"tag", tag, "topic", route.Topic, "key", route.Key, "msgID", route.MsgID)
		return
	}
	item, err := pipeline.NewItem(name, tag, route.Key, route.MsgID, route.Payload)
	if err != nil {
		logutil.Error("编排层编码下一跳载荷失败，消息已丢弃",
			"tag", tag, "topic", route.Topic, "node", name, "err", err)
		o.deadLetter(item, err)
		return
	}
	if err := o.nodes[name].buffer.Put(ctx, item); err != nil {
		if errors.Is(err, pipeline.ErrClosed) {
			logutil.Warn("编排层投递时缓冲已关闭，消息停在这一跳",
				"topic", route.Topic, "node", name, "item", item.ID)
			return
		}
		logutil.Error("编排层投递失败，消息停在这一跳",
			"topic", route.Topic, "node", name, "item", item.ID, "err", err)
		o.deadLetter(item, err)
		return
	}
	o.nodes[name].stage.Submit(route.Key)
}

func (o *orchestrator) deadLetter(item pipeline.Item, err error) {
	if o.onDeadLetter != nil {
		o.onDeadLetter(item, err)
	}
}

// admit 是入站的唯一入口：把一条事件放进前置，由前置补全后决定进哪条工作流。
// 前置还不知道 tag（哪条工作流），所以传空。
func (o *orchestrator) admit(ctx context.Context, key, msgID string, payload any) error {
	if o.router == nil {
		return fmt.Errorf("bot: 编排层没有挂载前置")
	}
	return o.put(ctx, o.router, "", key, msgID, payload)
}

// start 跳过前置，直接进某条工作流。给「已经在别处补全过」的第二个来源用
// （面板触发、定时任务、Webhook）——它们只需要业务链，不需要再过一遍入站与补全。
func (o *orchestrator) start(ctx context.Context, tag, key, msgID string, payload any) error {
	wf, ok := o.workflows[tag]
	if !ok {
		return fmt.Errorf("bot: 编排层没有名为 %q 的工作流", tag)
	}
	n, ok := o.nodes[wf.entry]
	if !ok {
		return fmt.Errorf("bot: 工作流 %q 的入口域 %q 没有挂载", tag, wf.entry)
	}
	return o.put(ctx, n, tag, key, msgID, payload)
}

// put 把一条载荷放进指定节点并唤醒它。tag 会写进 Item，跟着消息走到底。
func (o *orchestrator) put(ctx context.Context, n *node, tag, key, msgID string, payload any) error {
	item, err := pipeline.NewItem(n.name, tag, key, msgID, payload)
	if err != nil {
		return err
	}
	if err := n.buffer.Put(ctx, item); err != nil {
		return err
	}
	n.stage.Submit(key)
	return nil
}

// lookup 按名字找节点：业务域优先，找不到再比前置。
//
// 前置不在 nodes 里（它不是任何一条工作流的一部分），但它同样有队列。
// 指向它的查询不能静默返回 0——那和「没有积压」分不清。
func (o *orchestrator) lookup(name string) *node {
	if n, ok := o.nodes[name]; ok {
		return n
	}
	if o.router != nil && o.router.name == name {
		return o.router
	}
	return nil
}

// backlog 返回某个节点当前的积压条数。域用它判断"这一批之后还有没有活"，
// 不需要也不该去摸别人的队列。
//
// 断言不会失败：Stage 构造期就要求 Buffer 实现 Peeker（否则 panic）。
// 这里只依赖"数得清"这一件事，不把 Len 塞回 Buffer。
func (o *orchestrator) backlog(name, key string) int {
	n := o.lookup(name)
	if n == nil {
		return 0
	}
	p, ok := n.buffer.(pipeline.Peeker)
	if !ok {
		return 0
	}
	return p.Len(key)
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
