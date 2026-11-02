package pipeline

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"good-review-master/telemetry"
)

func stageConfig(batch, concurrency, retries int) func() StageConfig {
	return func() StageConfig {
		return StageConfig{Batch: batch, Concurrency: concurrency, Retries: retries}
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
			item := mustItem(t, StageSync, key, key+string(rune('0'+i)), struct{}{})
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
		item := mustItem(t, StageSync, "k", id, struct{}{})
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

// 名额只有 1 个时，已经闲下来的会话不该继续占着名额。
// 老实现里 worker 会挂着等数据直到 IdleTTL，这里 b、c 要等到超时才被处理。
func TestStage空闲会话不占名额(t *testing.T) {
	buf := NewMemoryBuffer(10, 100)
	handled := make(chan string, 10)
	stage := NewStage(StageOptions{
		Name:   "test",
		Buffer: buf,
		Config: stageConfig(1, 1, 0),
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

	put := func(key, id string) {
		item := mustItem(t, StageSync, key, id, struct{}{})
		if err := buf.Put(ctx, item); err != nil {
			t.Fatal(err)
		}
		stage.Submit(key)
	}

	// a 先干完活然后闲着，此时它不该占着唯一的名额。
	put("a", "a1")
	select {
	case id := <-handled:
		if id != "sync|a|a1" {
			t.Fatalf("处理了 %q", id)
		}
	case <-time.After(time.Second):
		t.Fatal("a 未被处理")
	}

	put("b", "b1")
	put("c", "c1")
	got := map[string]bool{}
	deadline := time.Now().Add(2 * time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		select {
		case id := <-handled:
			got[id] = true
		case <-time.After(5 * time.Millisecond):
		}
	}
	if !got["sync|b|b1"] || !got["sync|c|c1"] {
		t.Fatalf("空闲会话占住了名额，b/c 没被处理：%v", got)
	}
}

// 并发数改了池子跟着变，不用重启进程。
func TestStage并发数热调整(t *testing.T) {
	buf := NewMemoryBuffer(10, 100)
	var limit atomic.Int32
	limit.Store(1)
	stage := NewStage(StageOptions{
		Name:   "test",
		Buffer: buf,
		Config: func() StageConfig {
			return StageConfig{Batch: 1, Concurrency: int(limit.Load())}
		},
		Handler: func(context.Context, string, []Item) []FailedItem { return nil },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stage.Run(ctx)
	if got := stage.Workers(); got != 1 {
		t.Fatalf("初始协程数 = %d", got)
	}

	limit.Store(4)
	if err := buf.Put(ctx, mustItem(t, StageSync, "k", "1", struct{}{})); err != nil {
		t.Fatal(err)
	}
	stage.Submit("k")
	if got := stage.Workers(); got != 4 {
		t.Fatalf("调大后协程数 = %d", got)
	}

	limit.Store(2)
	stage.Submit("k")
	deadline := time.Now().Add(time.Second)
	for stage.Workers() != 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := stage.Workers(); got != 2 {
		t.Fatalf("调小后协程数 = %d", got)
	}
}

// 指标要真的跟着状态走：池子大小、排队会话数、正在处理的会话数、耗时直方图。
// 标签写错不会报错，只会多一条永远为 0 的时间线，所以这里把名字固定死。
func TestStage指标跟着状态走(t *testing.T) {
	const stageName = "metricstest"
	buf := NewMemoryBuffer(10, 10)
	handled := make(chan struct{}, 1)
	stage := NewStage(StageOptions{
		Name:   stageName,
		Buffer: buf,
		Config: stageConfig(1, 2, 0),
		Handler: func(context.Context, string, []Item) []FailedItem {
			handled <- struct{}{}
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stage.Run(ctx)

	gauge := func(g *prometheus.GaugeVec) float64 {
		return testutil.ToFloat64(g.WithLabelValues(stageName))
	}
	if got := gauge(telemetry.PipelineStageWorkers); got != 2 {
		t.Fatalf("workers = %v, want 2", got)
	}

	if err := buf.Put(ctx, mustItem(t, StageSync, "k", "1", struct{}{})); err != nil {
		t.Fatal(err)
	}
	stage.Submit("k")
	select {
	case <-handled:
	case <-time.After(time.Second):
		t.Fatal("消息未被处理")
	}

	// 交还之后：正在处理、排队、积压都应该归零。
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if gauge(telemetry.PipelineStageActive) == 0 &&
			gauge(telemetry.PipelineStageReady) == 0 &&
			gauge(telemetry.PipelineStageBacklog) == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := gauge(telemetry.PipelineStageActive); got != 0 {
		t.Fatalf("active = %v, want 0", got)
	}
	if got := gauge(telemetry.PipelineStageReady); got != 0 {
		t.Fatalf("ready = %v, want 0", got)
	}
	if got := gauge(telemetry.PipelineStageBacklog); got != 0 {
		t.Fatalf("backlog = %v, want 0", got)
	}
	if n := testutil.CollectAndCount(telemetry.PipelineStageBatchDurationSeconds); n == 0 {
		t.Fatal("批次耗时直方图没有记录")
	}
}

// 池里有多个协程，但同一个会话同时只能有一个在处理。
func TestStage同一会话不并发(t *testing.T) {
	buf := NewMemoryBuffer(100, 100)
	var running, overlapped atomic.Int32
	stage := NewStage(StageOptions{
		Name:   "test",
		Buffer: buf,
		Config: stageConfig(1, 3, 0),
		Handler: func(ctx context.Context, key string, items []Item) []FailedItem {
			if running.Add(1) > 1 {
				overlapped.Store(1)
			}
			time.Sleep(3 * time.Millisecond)
			running.Add(-1)
			return nil
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stage.Run(ctx)

	for i := 0; i < 20; i++ {
		item := mustItem(t, StageSync, "same", string(rune('a'+i)), struct{}{})
		if err := buf.Put(ctx, item); err != nil {
			t.Fatal(err)
		}
		stage.Submit("same")
	}

	deadline := time.Now().Add(2 * time.Second)
	for buf.Len("same") > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if buf.Len("same") != 0 {
		t.Fatalf("积压未处理完，剩余 %d", buf.Len("same"))
	}
	if overlapped.Load() == 1 {
		t.Fatal("同一个会话被两个协程同时处理")
	}
}
