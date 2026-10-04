package llm

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"good-review-master/logutil"
	"good-review-master/telemetry"

	"github.com/sony/gobreaker/v2"
	"golang.org/x/time/rate"
)

// ErrRateLimited 本地限速拦截：调用根本没有发出去。
var ErrRateLimited = errors.New("大模型调用被本地限速拦截")

// ErrCircuitOpen 熔断器处于打开状态：调用被立刻拒绝，不会等 llm_timeout_sec。
var ErrCircuitOpen = errors.New("大模型连续失败，已熔断")

// ResilientOptions 出站保护参数。
//
// 两项保护的风险不对称，所以缺省也不同：
//   - 限速默认**关闭**（RatePerSecond<=0）：它会在正常流量下拒掉请求，
//     只有在明确知道上游配额或账单上限时才该开；
//   - 熔断默认**开启**（BreakerFailures>0）：它只在已经连续失败若干次后才动作，
//     那时每次放开调用都要等满 llm_timeout_sec，开着是在保护群里的体验。
type ResilientOptions struct {
	RatePerSecond   float64       // 每秒允许的调用数；<=0 表示不限速
	RateLimitBurst  int           // 令牌桶容量；<=0 时按限速值向上取整、至少 1
	BreakerFailures int           // 连续失败多少次后熔断；<=0 表示不熔断
	BreakerCooldown time.Duration // 熔断后进入半开试探前的冷却时长
}

// resilientClient 给内层客户端包上限速与熔断。
//
// 用装饰器而不是把逻辑塞进 OpenAIAdapter：前者对 FakeLLM 之类的其它实现同样适用，
// 也让"怎么调"与"怎么保护"各自独立可测。
type resilientClient struct {
	inner   Client
	limiter *rate.Limiter                  // nil = 不限速
	breaker *gobreaker.CircuitBreaker[any] // nil = 不熔断
}

// NewResilient 包装内层客户端。opts 全为 0/负 时等价于原样返回内层实现。
func NewResilient(inner Client, opts ResilientOptions) Client {
	client := &resilientClient{inner: inner}

	if opts.RatePerSecond > 0 {
		burst := opts.RateLimitBurst
		if burst <= 0 {
			burst = int(math.Ceil(opts.RatePerSecond))
			if burst < 1 {
				burst = 1
			}
		}
		client.limiter = rate.NewLimiter(rate.Limit(opts.RatePerSecond), burst)
	}

	if opts.BreakerFailures > 0 {
		client.breaker = gobreaker.NewCircuitBreaker[any](gobreaker.Settings{
			Name:        "llm",
			MaxRequests: 1, // 半开时只放一个请求去试探，避免刚恢复就被打回
			Timeout:     opts.BreakerCooldown,
			ReadyToTrip: func(counts gobreaker.Counts) bool {
				return counts.ConsecutiveFailures >= uint32(opts.BreakerFailures)
			},
			// 关掉一个进程（Ctrl+C）会取消在途调用，那不是上游的故障。
			// 不排除的话，每次退出都会给熔断器记一笔失败，重启后头几个请求
			// 可能直接撞上"熔断中"——一个纯粹由正常操作制造出来的假故障。
			IsExcluded: func(err error) bool { return errors.Is(err, context.Canceled) },
			OnStateChange: func(name string, from, to gobreaker.State) {
				logutil.Warn("大模型熔断器状态变化", "name", name, "从", from.String(), "到", to.String())
			},
		})
	}

	return client
}

// SingleChat 见 Client 接口。
func (c *resilientClient) SingleChat(ctx context.Context, chatLog, systemPrompt string) (string, error) {
	var reply string
	err := c.guard(func() error {
		var callErr error
		reply, callErr = c.inner.SingleChat(ctx, chatLog, systemPrompt)
		return callErr
	})
	return reply, err
}

// MutiChatWithTool 见 Client 接口。
func (c *resilientClient) MutiChatWithTool(ctx context.Context, messages []Message, tools []Tool) (*ChatResponse, error) {
	var response *ChatResponse
	err := c.guard(func() error {
		var callErr error
		response, callErr = c.inner.MutiChatWithTool(ctx, messages, tools)
		return callErr
	})
	return response, err
}

// guard 依次过限速与熔断，最后才真正调用内层客户端。
func (c *resilientClient) guard(call func() error) error {
	// 限速用 Allow 而不是 Wait：等下去只会把池子里的 worker 占住，
	// 而这条链路本来就允许失败（调用方会回一句"稍后再试"）。
	// 快速失败同时把压力反馈给了群里——比无声堆积要好。
	if c.limiter != nil && !c.limiter.Allow() {
		telemetry.LLMRejectedTotal.WithLabelValues(telemetry.ReasonRateLimited).Inc()
		return ErrRateLimited
	}
	if c.breaker == nil {
		return call()
	}

	_, err := c.breaker.Execute(func() (any, error) { return nil, call() })
	if err == nil {
		return nil
	}
	if errors.Is(err, gobreaker.ErrOpenState) || errors.Is(err, gobreaker.ErrTooManyRequests) {
		telemetry.LLMRejectedTotal.WithLabelValues(telemetry.ReasonCircuitOpen).Inc()
		// 把底层错误一起带上：半开状态下"太多请求"与"还开着"是两种不同处境，
		// 排查时看到原始措辞比看到统一的"熔断中"有用。
		return fmt.Errorf("%w（%v）", ErrCircuitOpen, err)
	}
	return err
}

// 编译期断言：装饰器与真实适配器满足同一个接口。
var _ Client = (*resilientClient)(nil)
