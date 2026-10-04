package llm

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// stubClient 记录调用次数并返回可编程的结果，用来观察装饰器到底有没有把请求放进去。
type stubClient struct {
	mu      sync.Mutex
	calls   int
	reply   string
	failure error
}

func (s *stubClient) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubClient) record() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.failure
}

func (s *stubClient) SingleChat(context.Context, string, string) (string, error) {
	if err := s.record(); err != nil {
		return "", err
	}
	return s.reply, nil
}

func (s *stubClient) MutiChatWithTool(context.Context, []Message, []Tool) (*ChatResponse, error) {
	if err := s.record(); err != nil {
		return nil, err
	}
	return &ChatResponse{Content: s.reply}, nil
}

// TestResilient全零选项不改变行为：默认配置下装饰器必须完全透明。
// 这点很重要——它意味着"引入限速/熔断"这件事对没配它们的用户零影响。
func TestResilient全零选项不改变行为(t *testing.T) {
	inner := &stubClient{reply: "正常回复"}
	client := NewResilient(inner, ResilientOptions{})

	got, err := client.SingleChat(context.Background(), "日志", "提示词")
	if err != nil {
		t.Fatalf("不应报错：%v", err)
	}
	if got != "正常回复" {
		t.Fatalf("回复 = %q，期望 正常回复", got)
	}
	if inner.count() != 1 {
		t.Fatalf("内层应被调用 1 次，实际 %d", inner.count())
	}
}

// TestResilient限速拦截多余调用：桶容量 1 时，第二个立刻到达的请求应当被拒，
// 且**不能**落到内层客户端上——限速的意义就是不发出去。
func TestResilient限速拦截多余调用(t *testing.T) {
	inner := &stubClient{reply: "ok"}
	// 每秒 0.001 个 ≈ 16 分钟补一个令牌，测试期间等价于"只放行初始的 burst 个"
	client := NewResilient(inner, ResilientOptions{RatePerSecond: 0.001, RateLimitBurst: 1})

	if _, err := client.SingleChat(context.Background(), "a", "b"); err != nil {
		t.Fatalf("第一次调用应当放行：%v", err)
	}
	_, err := client.SingleChat(context.Background(), "a", "b")
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("第二次调用应被限速拦截，实际 err=%v", err)
	}
	if inner.count() != 1 {
		t.Fatalf("被拦下的请求不该落到内层，内层调用次数 = %d", inner.count())
	}
}

// TestResilient熔断在连续失败后打开 是这套保护里最核心的一条：
// 打开之后请求连内层都不进——模型坏掉时，用户等 120 秒超时和立刻看到"罢工"
// 是天差地别的体验。
func TestResilient熔断在连续失败后打开(t *testing.T) {
	inner := &stubClient{failure: errors.New("上游 500")}
	client := NewResilient(inner, ResilientOptions{
		BreakerFailures: 3,
		BreakerCooldown: time.Minute,
	})

	for i := 0; i < 3; i++ {
		if _, err := client.SingleChat(context.Background(), "a", "b"); !errors.Is(err, inner.failure) {
			t.Fatalf("第 %d 次调用应原样冒泡上游错误，实际 %v", i+1, err)
		}
	}
	if inner.count() != 3 {
		t.Fatalf("熔断前内层应被调用 3 次，实际 %d", inner.count())
	}

	_, err := client.SingleChat(context.Background(), "a", "b")
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("连续失败达到阈值后应熔断，实际 err=%v", err)
	}
	if inner.count() != 3 {
		t.Fatalf("熔断打开后请求不该落到内层，内层调用次数 = %d", inner.count())
	}
}

// TestResilient成功后重置失败计数：熔断看的是**连续**失败。
// 不重置的话，偶发失败累积到阈值就会把健康的上游也断开。
func TestResilient成功后重置失败计数(t *testing.T) {
	inner := &stubClient{failure: errors.New("偶发失败")}
	client := NewResilient(inner, ResilientOptions{
		BreakerFailures: 3,
		BreakerCooldown: time.Minute,
	})

	// 失败两次 → 成功一次（清空计数） → 再失败两次：始终不该熔断
	for i := 0; i < 2; i++ {
		_, _ = client.SingleChat(context.Background(), "a", "b")
	}
	inner.mu.Lock()
	inner.failure = nil
	inner.mu.Unlock()
	if _, err := client.SingleChat(context.Background(), "a", "b"); err != nil {
		t.Fatalf("中间的成功应能通过：%v", err)
	}
	inner.mu.Lock()
	inner.failure = errors.New("偶发失败")
	inner.mu.Unlock()
	for i := 0; i < 2; i++ {
		if _, err := client.SingleChat(context.Background(), "a", "b"); !errors.Is(err, inner.failure) {
			t.Fatalf("重置后应仍能正常调用，实际 %v", err)
		}
	}
}

// TestResilient关机取消不计入熔断 防一个纯粹由正常操作造出来的假故障：
// 每次 Ctrl+C 都会取消在途调用，若把它算作失败，重启后头几个请求
// 就可能直接撞上"熔断中"——而实际上上游一直好好的。
func TestResilient关机取消不计入熔断(t *testing.T) {
	inner := &stubClient{failure: context.Canceled}
	client := NewResilient(inner, ResilientOptions{
		BreakerFailures: 2,
		BreakerCooldown: time.Minute,
	})

	for i := 0; i < 4; i++ {
		if _, err := client.SingleChat(context.Background(), "a", "b"); !errors.Is(err, context.Canceled) {
			t.Fatalf("第 %d 次调用应返回取消错误，实际 %v", i+1, err)
		}
	}
	if inner.count() != 4 {
		t.Fatalf("取消不该计入熔断，内层应被调用 4 次，实际 %d", inner.count())
	}
}

// TestResilient熔断也作用于多轮对话：两条入口都得过同一道闸，
// 漏掉一条就等于给了模型一条绕开保护的旁路。
func TestResilient熔断也作用于多轮对话(t *testing.T) {
	inner := &stubClient{failure: errors.New("上游 500")}
	client := NewResilient(inner, ResilientOptions{
		BreakerFailures: 1,
		BreakerCooldown: time.Minute,
	})

	if _, err := client.MutiChatWithTool(context.Background(), nil, nil); err == nil {
		t.Fatal("第一次调用应冒泡上游错误")
	}
	_, err := client.MutiChatWithTool(context.Background(), nil, nil)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("多轮对话入口也应被熔断拦住，实际 %v", err)
	}
	if inner.count() != 1 {
		t.Fatalf("内层调用次数 = %d，期望 1", inner.count())
	}
}
