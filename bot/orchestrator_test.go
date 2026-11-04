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

// assertTopicsCovered 断言编排层认识这些回执：每一类要么连到某个域、
// 要么是某条工作流的名字（前置回它），要么显式声明为终点。
// 三样都没有的话，运行期会被当成装配错误丢弃。加了一条回执却没接上时，这里会先报错。
func assertTopicsCovered(t *testing.T, o *orchestrator, topics ...string) {
	t.Helper()
	for _, topic := range topics {
		_, linked := o.edges[topic]
		_, workflow := o.workflows[topic]
		if !linked && !workflow && !o.exits[topic] {
			t.Fatalf("回执 %q 既没连线、也不是工作流名、也没声明为终点，走到这里会被丢弃", topic)
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
	o.connect("next", "b")
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
	case id := <-delivered:
		if id != "b|k1|m1" {
			t.Fatalf("投递到的条目 ID = %q，want b|k1|m1（Item.Stage 应记目标节点）", id)
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
	o.connect("next", "b")
	o.closeBuffers()

	o.dispatch(context.Background(), []Route{
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
	o.connect("next", "nope")
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
	o.exit("done")
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
			if order == "先连线后终点" {
				o.connect("next", "b")
				o.exit("next")
				return
			}
			o.exit("next")
			o.connect("next", "b")
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

// Test工作流名和去向重名会panic 两者都在 Route.Topic 里，同名就分不清是
// “进工作流”还是“去下一个域”。两种书写顺序都要在构造期炸。
func Test工作流名和去向重名会panic(t *testing.T) {
	for _, order := range []string{"先连线后工作流", "先工作流后连线"} {
		t.Run(order, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("工作流名与去向重名应当 panic")
				}
			}()
			o := newOrchestrator(nil)
			handler := func(context.Context, string, []pipeline.Item) Outcome { return Outcome{} }
			o.mount("a", handler, testStageConfig, 4, 4)
			o.mount("b", handler, testStageConfig, 4, 4)
			if order == "先连线后工作流" {
				o.connect("same", "b")
				o.workflow("same", "a")
				return
			}
			o.workflow("same", "a")
			o.connect("same", "b")
		})
	}
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
