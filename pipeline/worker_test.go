package pipeline

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func stageConfig(batch, concurrency, retries int) func() StageConfig {
	return func() StageConfig {
		return StageConfig{Batch: batch, Concurrency: concurrency, Retries: retries, IdleTTL: time.Minute}
	}
}

func TestStage同会话串行与跨会话并行(t *testing.T) {
	buf := NewMemoryBuffer(100, 100)
	var mu sync.Mutex
	order := map[string][]string{}
	var active, maxActive atomic.Int32
	stage := NewStage(StageOptions{
		Name:   "test",
		Buffer: buf,
		Config: stageConfig(4, 2, 0),
		Handler: func(ctx context.Context, key string, items []Item) []FailedItem {
			now := active.Add(1)
			for {
				old := maxActive.Load()
				if now <= old || maxActive.CompareAndSwap(old, now) {
					break
				}
			}
			defer active.Add(-1)
			mu.Lock()
			for _, item := range items {
				order[key] = append(order[key], item.ID)
			}
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stage.Run(ctx)

	for _, key := range []string{"a", "b"} {
		for i := 0; i < 4; i++ {
			item := mustItem(t, StageSync, key, key+string(rune('0'+i)), InboundEvent{})
			if err := buf.Put(ctx, item); err != nil {
				t.Fatal(err)
			}
			stage.Submit(key)
		}
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(order["a"]) == 4 && len(order["b"]) == 4
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order["a"]) != 4 || len(order["b"]) != 4 {
		t.Fatalf("处理数量不对：%+v", order)
	}
	for _, key := range []string{"a", "b"} {
		for i, id := range order[key] {
			want := "sync|" + key + "|" + key + string(rune('0'+i))
			if id != want {
				t.Fatalf("%s 顺序错误：%v", key, order[key])
			}
		}
	}
	if maxActive.Load() > 2 {
		t.Fatalf("全局并发超限：%d", maxActive.Load())
	}
}

func TestStage批量取数与重试死信(t *testing.T) {
	buf := NewMemoryBuffer(100, 100)
	var attempts atomic.Int32
	var dead []string
	stage := NewStage(StageOptions{
		Name:   "test",
		Buffer: buf,
		Config: stageConfig(3, 1, 1),
		Handler: func(ctx context.Context, key string, items []Item) []FailedItem {
			var failed []FailedItem
			for _, item := range items {
				if item.ID == "sync|k|bad" {
					attempts.Add(1)
					failed = append(failed, FailedItem{Item: item, Err: errors.New("boom")})
				}
			}
			return failed
		},
		OnDeadLetter: func(item Item, err error) { dead = append(dead, item.ID) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stage.Run(ctx)

	for _, id := range []string{"good1", "good2", "bad"} {
		item := mustItem(t, StageSync, "k", id, InboundEvent{})
		if err := buf.Put(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	stage.Submit("k")

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(dead) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	// 初次 + 1 次重试 = 2 次，然后进死信。
	if attempts.Load() != 2 {
		t.Fatalf("重试次数 = %d, want 2", attempts.Load())
	}
	if len(dead) != 1 || dead[0] != "sync|k|bad" {
		t.Fatalf("死信 = %v", dead)
	}
	if buf.Len("k") != 0 {
		t.Fatalf("失败项应已出队，剩余 %d", buf.Len("k"))
	}
}

func TestStage空闲回收后重新拉起(t *testing.T) {
	buf := NewMemoryBuffer(10, 10)
	handled := make(chan string, 10)
	stage := NewStage(StageOptions{
		Name:   "test",
		Buffer: buf,
		Config: func() StageConfig {
			return StageConfig{Batch: 1, Concurrency: 1, IdleTTL: 30 * time.Millisecond}
		},
		Handler: func(ctx context.Context, key string, items []Item) []FailedItem {
			for _, item := range items {
				handled <- item.ID
			}
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stage.Run(ctx)

	put := func(id string) {
		item := mustItem(t, StageSync, "k", id, InboundEvent{})
		if err := buf.Put(ctx, item); err != nil {
			t.Fatal(err)
		}
		stage.Submit("k")
	}
	put("one")
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("第一条未被处理")
	}

	deadline := time.Now().Add(time.Second)
	for stage.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if stage.Active() != 0 {
		t.Fatal("空闲后 worker 应退出")
	}

	put("two")
	select {
	case id := <-handled:
		if id != "sync|k|two" {
			t.Fatalf("重新拉起后处理了 %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("空闲回收后新消息未重新拉起 worker")
	}
}
