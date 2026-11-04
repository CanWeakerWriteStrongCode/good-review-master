package bot

import (
	"context"
	"testing"
	"time"

	"good-review-master/pipeline"
)

func testStageConfig() pipeline.StageConfig {
	return pipeline.StageConfig{Batch: 1, Concurrency: 1}
}

// assertTopicsCovered 断言某条工作流认识这些回执：每一类要么连到某个域、
// 要么显式声明为终点。两样都没有的话，运行期会被当成装配错误丢弃。
// 加了一条回执却没接上时，这里会先报错。
func assertTopicsCovered(t *testing.T, o *orchestrator, workflowName string, topics ...string) {
	t.Helper()
	wf, ok := o.workflows[workflowName]
	if !ok {
		t.Fatalf("工作流 %q 没有声明", workflowName)
	}
	for _, topic := range topics {
		if _, linked := wf.next[topic]; !linked && !wf.ends[topic] {
			t.Fatalf("工作流 %q 的回执 %q 既没连线、也没声明为终点，走到这里会被丢弃", workflowName, topic)
		}
	}
}

// Test编排层把回执投给下一个域 验证"域回消息、编排层转发"这条闭环：
// 域 A 只回一个 topic，消息最终落到 topic 连到的域 B，且 Item.ID 记的是目标节点。
func Test编排层把回执投给下一个域(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	delivered := make(chan string, 8)
	o := newOrchestrator(nil)
	o.mount("a", func(_ context.Context, key string, _ []pipeline.Item) Outcome {
		return Outcome{Routes: []Route{{Topic: "next", Key: key, MsgID: "m1", Payload: struct{}{}}}}
	}, testStageConfig, 8, 8)
	o.mount("b", func(_ context.Context, _ string, items []pipeline.Item) Outcome {
		delivered <- items[0].ID
		return Outcome{}
	}, testStageConfig, 8, 8)
	o.workflow("w", "a")
	o.connect("w", "next", "b")

	o.run(ctx)
	defer func() {
		o.closeBuffers()
		o.closeStages()
		o.wait()
	}()

	if err := o.start(ctx, "w", "k1", "x1", struct{}{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case id := <-delivered:
		if id != "b|w|k1|m1" {
			t.Fatalf("投递到的条目 ID = %q，want b|w|k1|m1（stage 是目标节点，w 是工作流 tag）", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("回执没有被投到下游域")
	}
}

// Test编排层没连线的去向不投递 既没连线也没声明终点的 topic 只丢这一条，不能 panic，
// 也不能当成死信报警——那是装配时漏了，不是运行期失败。终点必须由链显式声明，
// 不能靠"没连线"推断，否则漏连线的后果就从报错变成静默少走一步。
func Test编排层没连线的去向不投递(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dead := 0
	reached := make(chan struct{}, 1)
	o := newOrchestrator(func(pipeline.Item, error) { dead++ })
	o.mount("a", func(_ context.Context, key string, _ []pipeline.Item) Outcome {
		return Outcome{Routes: []Route{{Topic: "没有这条线", Key: key, MsgID: "m1", Payload: struct{}{}}}}
	}, testStageConfig, 8, 8)
	o.mount("b", func(_ context.Context, _ string, _ []pipeline.Item) Outcome {
		reached <- struct{}{}
		return Outcome{}
	}, testStageConfig, 8, 8)
	o.workflow("w", "a")

	o.run(ctx)
	defer func() {
		o.closeBuffers()
		o.closeStages()
		o.wait()
	}()

	if err := o.start(ctx, "w", "k1", "x1", struct{}{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-reached:
		t.Fatal("未连线的去向不应投到任何域")
	case <-time.After(300 * time.Millisecond):
	}
	if dead != 0 {
		t.Fatalf("未连线不该记死信，实际 %d 条", dead)
	}
}

// Test编排层投递到已关闭的缓冲不报警 关停排空时缓冲先关、域还在跑，
// 这时的投递失败是预期结果，报警的话每次正常重启都会刷一堆假死信。
func Test编排层投递到已关闭的缓冲不报警(t *testing.T) {
	dead := 0
	o := newOrchestrator(func(pipeline.Item, error) { dead++ })
	o.mount("b", func(context.Context, string, []pipeline.Item) Outcome { return Outcome{} },
		testStageConfig, 4, 4)
	o.workflow("w", "b")
	o.connect("w", "next", "b")
	o.closeBuffers()

	o.dispatch(context.Background(), "w", []Route{
		{Topic: "next", Key: "k", MsgID: "m", Payload: struct{}{}},
	})
	if dead != 0 {
		t.Fatalf("关停期投递失败不该报警，实际 %d 条", dead)
	}
}

// Test编排层连线指向未挂载的域会panic 装配错误要在启动时就炸，
// 不能等某条消息走到那里才发现。
func Test编排层连线指向未挂载的域会panic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("连线指向未挂载的域应当 panic")
		}
	}()
	o := newOrchestrator(nil)
	o.mount("a", func(context.Context, string, []pipeline.Item) Outcome { return Outcome{} },
		testStageConfig, 4, 4)
	o.workflow("w", "a")
	o.connect("w", "next", "nope")
}

// Test编排层声明为终点的去向不投递 终点是链的声明，不是域的沉默：
// 域照常交回执，链说这一类到此为止，消息就停在这里——不报警，也不记死信。
// 同一个域挂到另一条链上、那条链没把它声明为终点，就能继续往下走，域不用改。
func Test编排层声明为终点的去向不投递(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dead := 0
	reached := make(chan struct{}, 1)
	o := newOrchestrator(func(pipeline.Item, error) { dead++ })
	o.mount("a", func(_ context.Context, key string, _ []pipeline.Item) Outcome {
		return Outcome{Routes: []Route{{Topic: "done", Key: key, MsgID: "m1", Payload: struct{}{}}}}
	}, testStageConfig, 8, 8)
	o.mount("b", func(_ context.Context, _ string, _ []pipeline.Item) Outcome {
		reached <- struct{}{}
		return Outcome{}
	}, testStageConfig, 8, 8)
	o.workflow("w", "a")
	o.exit("w", "done")

	o.run(ctx)
	defer func() {
		o.closeBuffers()
		o.closeStages()
		o.wait()
	}()

	if err := o.start(ctx, "w", "k1", "x1", struct{}{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	select {
	case <-reached:
		t.Fatal("声明为终点的去向不应投到任何域")
	case <-time.After(300 * time.Millisecond):
	}
	if dead != 0 {
		t.Fatalf("终点是正常结束，不该记死信，实际 %d 条", dead)
	}
}

// Test编排层同一个去向既连线又称终点会panic 两件事互斥，写在一起说明装配时想岔了。
// 两种书写顺序都要在构造期炸，不能因为后一句覆写前一句而静默生效。
func Test编排层同一个去向既连线又称终点会panic(t *testing.T) {
	for _, order := range []string{"先连线后终点", "先终点后连线"} {
		t.Run(order, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("既连线又声明终点应当 panic")
				}
			}()
			o := newOrchestrator(nil)
			o.mount("b", func(context.Context, string, []pipeline.Item) Outcome { return Outcome{} },
				testStageConfig, 4, 4)
			o.workflow("w", "b")
			if order == "先连线后终点" {
				o.connect("w", "next", "b")
				o.exit("w", "next")
				return
			}
			o.exit("w", "next")
			o.connect("w", "next", "b")
		})
	}
}

// Test前置把消息路由到不同工作流 是拆分后的核心：前置不属于任何一条工作流，
// 它只回一个工作流名，编排层负责翻译成那条工作流的入口域。
// 两条工作流各自收到自己的那一条，没有串到对方。
func Test前置把消息路由到不同工作流(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chat := make(chan string, 4)
	command := make(chan string, 4)

	o := newOrchestrator(nil)
	// 前置：按 key 决定走哪条（真实实现里看的是“是不是管理指令”）。
	o.mountRouter(pipeline.StageSync, func(_ context.Context, key string, items []pipeline.Item) Outcome {
		workflow := WorkflowChat
		if key == "cmd" {
			workflow = WorkflowCommand
		}
		return Outcome{Routes: []Route{{
			Topic: workflow, Key: key, MsgID: items[0].ID, Payload: struct{}{},
		}}}
	}, testStageConfig, 8, 8)
	o.mount("chat_entry", func(_ context.Context, key string, _ []pipeline.Item) Outcome {
		chat <- key
		return Outcome{}
	}, testStageConfig, 8, 8)
	o.mount("cmd_entry", func(_ context.Context, key string, _ []pipeline.Item) Outcome {
		command <- key
		return Outcome{}
	}, testStageConfig, 8, 8)
	o.workflow(WorkflowChat, "chat_entry")
	o.workflow(WorkflowCommand, "cmd_entry")

	o.run(ctx)
	defer func() {
		o.closeBuffers()
		o.closeStages()
		o.wait()
	}()

	if err := o.admit(ctx, "chat", "m1", struct{}{}); err != nil {
		t.Fatalf("admit chat: %v", err)
	}
	if err := o.admit(ctx, "cmd", "m2", struct{}{}); err != nil {
		t.Fatalf("admit cmd: %v", err)
	}

	for _, want := range []struct {
		name string
		ch   chan string
		key  string
	}{
		{"聊天工作流", chat, "chat"},
		{"指令工作流", command, "cmd"},
	} {
		select {
		case got := <-want.ch:
			if got != want.key {
				t.Fatalf("%s 收到的 key = %q，want %q", want.name, got, want.key)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s 没有收到消息", want.name)
		}
	}
}

// Test没挂前置时admit报错 前置是入站的唯一入口；没挂就是装配漏了，
// 此时不能静默丢消息。
func Test没挂前置时admit报错(t *testing.T) {
	o := newOrchestrator(nil)
	if err := o.admit(context.Background(), "k", "m", struct{}{}); err == nil {
		t.Fatal("没有前置时 admit 应当返回错误")
	}
}

// Test两条工作流各自解析同名去向 是 topic + tag 模型的核心：
// 去向只在所属的那条工作流里查，所以 chat 和 command 各声明一个 "done" 互不干扰。
// 消息带着自己的 tag 走到底，投到各自工作流的下一个域。
func Test两条工作流各自解析同名去向(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	got := make(chan string, 4)
	o := newOrchestrator(nil)

	// 两条工作流的入口都只回一个同名去向 "done"。
	enter := func(name string) DomainHandler {
		return func(_ context.Context, key string, _ []pipeline.Item) Outcome {
			return Outcome{Routes: []Route{{Topic: "done", Key: key, MsgID: name, Payload: struct{}{}}}}
		}
	}
	o.mount("chat_entry", enter("chat"), testStageConfig, 8, 8)
	o.mount("cmd_entry", enter("command"), testStageConfig, 8, 8)
	o.mount("chat_next", func(_ context.Context, _ string, items []pipeline.Item) Outcome {
		got <- "chat:" + items[0].Tag
		return Outcome{}
	}, testStageConfig, 8, 8)
	o.mount("cmd_next", func(_ context.Context, _ string, items []pipeline.Item) Outcome {
		got <- "command:" + items[0].Tag
		return Outcome{}
	}, testStageConfig, 8, 8)

	o.workflow(WorkflowChat, "chat_entry")
	o.connect(WorkflowChat, "done", "chat_next")
	o.workflow(WorkflowCommand, "cmd_entry")
	o.connect(WorkflowCommand, "done", "cmd_next")

	o.run(ctx)
	defer func() {
		o.closeBuffers()
		o.closeStages()
		o.wait()
	}()

	if err := o.start(ctx, WorkflowChat, "k1", "m1", struct{}{}); err != nil {
		t.Fatalf("start chat: %v", err)
	}
	if err := o.start(ctx, WorkflowCommand, "k2", "m2", struct{}{}); err != nil {
		t.Fatalf("start command: %v", err)
	}

	want := map[string]bool{"chat:chat": false, "command:command": false}
	for range want {
		select {
		case v := <-got:
			if _, ok := want[v]; !ok {
				t.Fatalf("收到了不属于任何工作流的投递：%q（tag 没跟着消息走或串线了）", v)
			}
			want[v] = true
		case <-time.After(2 * time.Second):
			t.Fatal("同名去向没有被投递到各自工作流的下一站")
		}
	}
	for v, seen := range want {
		if !seen {
			t.Fatalf("%s 没有被投递", v)
		}
	}
}

// Test同一个工作流里同一个去向往两处连会panic 两条路径抢同一个 tag，
// 后写覆盖先写会让装配看起来成功、运行时分流。
func Test同一个工作流里同一个去向往两处连会panic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("同一个去向连到两个域应当 panic")
		}
	}()
	o := newOrchestrator(nil)
	handler := func(context.Context, string, []pipeline.Item) Outcome { return Outcome{} }
	o.mount("a", handler, testStageConfig, 4, 4)
	o.mount("b", handler, testStageConfig, 4, 4)
	o.workflow("w", "a")
	o.connect("w", "done", "a")
	o.connect("w", "done", "b")
}

// Test一批里混两条工作流时按tag分开处理 一个域被多条工作流复用时，
// 一批里可能有两种 tag。消费端必须按 tag 分派（MQ 消费者订阅多个 tag 的做法），
// 而不是按第一条算——否则第二条会被送到另一条工作流的下游。
func Test一批里混两条工作流时按tag分开处理(t *testing.T) {
	ctx := context.Background()

	var calls []string
	o := newOrchestrator(nil)
	shared := func(_ context.Context, key string, items []pipeline.Item) Outcome {
		calls = append(calls, items[0].Tag)
		return Outcome{Routes: []Route{{Topic: "done", Key: key, MsgID: "m", Payload: struct{}{}}}}
	}
	o.mount("shared", shared, testStageConfig, 8, 8)
	o.mount("w1_next", func(context.Context, string, []pipeline.Item) Outcome { return Outcome{} },
		testStageConfig, 8, 8)
	o.mount("w2_next", func(context.Context, string, []pipeline.Item) Outcome { return Outcome{} },
		testStageConfig, 8, 8)
	o.workflow("w1", "shared")
	o.connect("w1", "done", "w1_next")
	o.workflow("w2", "shared")
	o.connect("w2", "done", "w2_next")

	items := []pipeline.Item{
		testItem(t, "shared", "w1", "k1", "m1"),
		testItem(t, "shared", "w2", "k1", "m2"),
	}
	o.consume(ctx, "k1", items, shared)

	if got := o.backlog("w1_next", "k1"); got != 1 {
		t.Fatalf("w1 的消息应投到 w1_next，实际积压 %d", got)
	}
	if got := o.backlog("w2_next", "k1"); got != 1 {
		t.Fatalf("w2 的消息应投到 w2_next，实际积压 %d", got)
	}
	if len(calls) != 2 || calls[0] != "w1" || calls[1] != "w2" {
		t.Fatalf("handler 按 tag 分组的调用 = %v，want [w1 w2]", calls)
	}
}

// testItem 造一条带 tag 的条目，模拟从前置投下来的消息。
func testItem(t *testing.T, stage, tag, key, id string) pipeline.Item {
	t.Helper()
	it, err := pipeline.NewItem(stage, tag, key, id, struct{}{})
	if err != nil {
		t.Fatalf("NewItem: %v", err)
	}
	return it
}

// Test工作流入口没挂载会panic 声明了工作流却没有这个入口域，说明挂载与声明脱节。
func Test工作流入口没挂载会panic(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("工作流入口未挂载应当 panic")
		}
	}()
	o := newOrchestrator(nil)
	o.workflow("w", "nope")
}
