package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"good-review-master/logutil"
)

// TestMain 初始化 logger 并隔离工作目录：
// Informer 出错时会打 WARN，而 logutil 未初始化是 nil 指针一调就 panic；
// 同时 SetupLogger 会在 cwd 下建 log/，不隔离就会污染源码树。
func TestMain(m *testing.M) {
	original, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	temp, err := os.MkdirTemp("", "store-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(temp); err != nil {
		panic(err)
	}
	logutil.SetupLogger()

	code := m.Run()

	logutil.Close()
	_ = os.Chdir(original)
	_ = os.RemoveAll(temp)
	os.Exit(code)
}

// fakeSource 手动控制"变了"的信号，让用例不依赖文件与时间。
type fakeSource struct {
	changed chan struct{}
}

func newFakeSource() *fakeSource {
	return &fakeSource{changed: make(chan struct{}, 4)}
}

func (f *fakeSource) Changed(ctx context.Context) <-chan struct{} { return f.changed }

func (f *fakeSource) signal() { f.changed <- struct{}{} }

func TestStoreGet返回最新快照(t *testing.T) {
	first := 1
	target := NewStore(&first)
	if got := *target.Get(); got != 1 {
		t.Fatalf("初始值应为 1，实际 %d", got)
	}
	second := 2
	target.replace(&second)
	if got := *target.Get(); got != 2 {
		t.Fatalf("替换后应为 2，实际 %d", got)
	}
}

// TestInformer收到信号后重读并通知 handler 是这套机制的核心路径。
func TestInformer收到信号后重读并通知(t *testing.T) {
	initial := "v1"
	target := NewStore(&initial)
	source := newFakeSource()

	callCount := 0
	var mu sync.Mutex
	var observedOld, observedNew string
	list := func() (*string, error) {
		mu.Lock()
		defer mu.Unlock()
		callCount++
		next := "v2"
		return &next, nil
	}

	informer := NewInformer(target, list, source)
	informer.AddEventHandler(func(old, next *string) {
		observedOld, observedNew = *old, *next
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = informer.Run(ctx) }()

	source.signal()
	waitFor(t, func() bool { return *target.Get() == "v2" }, "快照未被替换")

	if observedOld != "v1" || observedNew != "v2" {
		t.Fatalf("handler 应收到 old=v1/new=v2，实际 old=%q new=%q", observedOld, observedNew)
	}
	mu.Lock()
	defer mu.Unlock()
	if callCount != 1 {
		t.Fatalf("list 应被调用 1 次，实际 %d", callCount)
	}
}

// TestInformer重读失败时保留旧快照 是最关键的一条：
// 用户改配置改到一半是常态，这时宁可继续用上一份可用配置，
// 也不能把系统推进"没有配置"的状态。
func TestInformer重读失败时保留旧快照(t *testing.T) {
	initial := "good"
	target := NewStore(&initial)
	source := newFakeSource()

	informer := NewInformer(target, func() (*string, error) {
		return nil, errors.New("磁盘上的 YAML 语法错了")
	}, source)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = informer.Run(ctx) }()

	source.signal()
	// 给循环足够时间跑完这次失败的重读
	time.Sleep(100 * time.Millisecond)

	if got := *target.Get(); got != "good" {
		t.Fatalf("重读失败后应保留旧快照 good，实际 %q", got)
	}
}

// TestInformer的handler panic不会带走循环 防止一次坏回调让配置永远不再更新。
func TestInformer的handler_panic不会带走循环(t *testing.T) {
	initial := "v1"
	target := NewStore(&initial)
	source := newFakeSource()

	version := 0
	list := func() (*string, error) {
		version++
		next := "v" + string(rune('0'+version))
		return &next, nil
	}

	informer := NewInformer(target, list, source)
	informer.AddEventHandler(func(old, next *string) { panic("回调写炸了") })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = informer.Run(ctx) }()

	source.signal()
	waitFor(t, func() bool { return *target.Get() == "v1" }, "第一次重读未生效")

	// 循环若被 panic 带走，这次信号不会有人处理
	source.signal()
	waitFor(t, func() bool { return *target.Get() == "v2" }, "handler panic 之后循环没继续跑")
}

func TestInformer手动Reload(t *testing.T) {
	initial := "v1"
	target := NewStore(&initial)

	next := "v2"
	informer := NewInformer(target, func() (*string, error) { return &next, nil }, newFakeSource())
	informer.Reload()

	if got := *target.Get(); got != "v2" {
		t.Fatalf("手动 Reload 应立即生效，实际 %q", got)
	}
}

// TestFilePoller能发现文件变化 用真实文件系统跑一遍轮询源。
func TestFilePoller能发现文件变化(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("a: 1\n"), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}

	poller := NewFilePoller(20*time.Millisecond, path)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changed := poller.Changed(ctx)

	// 初始状态没有信号
	select {
	case <-changed:
		t.Fatal("未改动就发了信号")
	case <-time.After(100 * time.Millisecond):
	}

	if err := os.WriteFile(path, []byte("a: 2\n"), 0o600); err != nil {
		t.Fatalf("改写失败：%v", err)
	}

	select {
	case <-changed:
	case <-time.After(2 * time.Second):
		t.Fatal("文件改了却没有发出信号")
	}
}

func waitFor(t *testing.T, condition func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(message)
}
