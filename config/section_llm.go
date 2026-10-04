package config

import (
	"fmt"
	"strings"

	"good-review-master/logutil"

	"gopkg.in/yaml.v3"
)

// llmSection 大模型配置（config.yaml 的 llm 段）
type llmSection struct {
	Provider  string `yaml:"provider"`
	APIKey    string `yaml:"api_key"` // 兼容旧位置；新配置放 secret.yaml
	APIBase   string `yaml:"api_base"`
	ModelName string `yaml:"model_name"`

	CacheHitCost  float64 `yaml:"cache_hit_cost"`
	CacheMissCost float64 `yaml:"cache_miss_cost"`

	MaxContextTokens int     `yaml:"max_context_tokens"`
	ImageMax         int     `yaml:"image_max"`
	Temperature      float64 `yaml:"temperature"`
	TopP             float64 `yaml:"top_p"`

	RateLimitPerSec    float64 `yaml:"rate_limit_per_sec"`
	RateLimitBurst     int     `yaml:"rate_limit_burst"`
	BreakerFailures    int     `yaml:"breaker_failures"`
	BreakerCooldownSec int     `yaml:"breaker_cooldown_sec"`
}

// 缺省值。cache_hit_cost / cache_miss_cost 是相对值，用于自动计算扩展阈值；
// 缺省按 deepseek 8/17 调价后的命中/未命中比 ≈ 1/30。
const (
	defaultCacheHitCost     = 0.033
	defaultCacheMissCost    = 1.0
	defaultMaxContextTokens = 50000

	// defaultBreakerFailures / defaultBreakerCooldownSec 熔断缺省：
	// 连续 5 次失败就打开，冷却 30 秒后进入半开试探。
	//
	// 熔断默认**开着**（限速默认关着），因为两者的风险不对称：
	// 限速会在正常流量下拒掉请求，必须在用户明确知道自己在干什么时才开；
	// 而熔断只在"已经连续失败 5 次"时才动作——那时每一次放开调用都要等满
	// llm_timeout_sec（默认 120 秒），开着它反而是在保护群里的体验。
	defaultBreakerFailures    = 5
	defaultBreakerCooldownSec = 30
)

// supportedProviders 支持的 provider 白名单。加新 provider 时要同时改 app/build.go 的分支。
var supportedProviders = []string{"openai"}

func (s *llmSection) Name() string { return "llm" }

func (s *llmSection) Unmarshal(document *yaml.Node) error {
	return decodeSection(document, s.Name(), s)
}

func (s *llmSection) SetDefaults() {
	s.Provider = strings.ToLower(strings.TrimSpace(s.Provider))
	if s.Provider == "" {
		s.Provider = "openai"
	}
	s.APIBase = strings.TrimSpace(s.APIBase)
	s.ModelName = strings.TrimSpace(s.ModelName)
	if s.CacheHitCost <= 0 {
		s.CacheHitCost = defaultCacheHitCost
	}
	if s.CacheMissCost <= 0 {
		s.CacheMissCost = defaultCacheMissCost
	}
	if s.MaxContextTokens <= 0 {
		s.MaxContextTokens = defaultMaxContextTokens
	}
	if s.BreakerFailures == 0 {
		// 只把"没写"（0）当作缺省；负数留给用户显式关闭（见 Validate 的说明）
		s.BreakerFailures = defaultBreakerFailures
	}
	if s.BreakerCooldownSec == 0 {
		s.BreakerCooldownSec = defaultBreakerCooldownSec
	}
}

// Validate 分两档，分界线是"这个值错了，程序还能不能跑"：
//
//   - 硬错误（返回 error，启动失败）：provider 不在白名单（走不到任何分支）、
//     护栏数值非法（下游直接 panic 或功能失效）
//   - 告警（继续启动）：模型名/地址缺失、采样参数越界——这些要么上游会明确报错，
//     要么本来就允许留空。把它们升级成启动失败，会让"存量配置少写一个可选字段"
//     变成打不开，代价大于收益
func (s *llmSection) Validate() error {
	if err := validateProvider(s.Provider); err != nil {
		return err
	}
	if s.MaxContextTokens <= 0 {
		return positiveInt("llm.max_context_tokens", s.MaxContextTokens,
			"它是防止上下文顶爆的护栏，超限会强制重置缓存窗口")
	}

	if s.ModelName == "" {
		logutil.Warn("llm.model_name 未配置，请求里会带空模型名，上游多半直接拒绝")
	}
	if s.APIBase == "" {
		logutil.Warn("llm.api_base 未配置，将使用 SDK 的默认地址；自建/代理网关请显式填写")
	} else if err := validateEndpoint("llm.api_base", s.APIBase); err != nil {
		logutil.Warn("llm.api_base 看起来不像能用的端点", "err", err)
	}
	if s.Temperature < 0 || s.Temperature > 2 {
		logutil.Warn("llm.temperature 超出常见区间 0~2", "值", s.Temperature)
	}
	if s.TopP < 0 || s.TopP > 1 {
		logutil.Warn("llm.top_p 超出区间 0~1", "值", s.TopP)
	}
	if s.RateLimitPerSec < 0 {
		return fmt.Errorf("llm.rate_limit_per_sec 不能为负（当前 %v）：0 表示不限速，正数表示每秒允许的调用数", s.RateLimitPerSec)
	}
	// -1 是"关闭熔断"的哨兵值，沿用本配置里 retry_interval_sec 已有的写法。
	if s.BreakerFailures < -1 {
		return fmt.Errorf("llm.breaker_failures 只能为 -1（关闭熔断）、0（用缺省 %d）或正整数（连续失败多少次后熔断），当前是 %d",
			defaultBreakerFailures, s.BreakerFailures)
	}
	if s.BreakerCooldownSec < 0 {
		return fmt.Errorf("llm.breaker_cooldown_sec 不能为负（当前 %d）：它是熔断后进入半开试探前的冷却秒数",
			s.BreakerCooldownSec)
	}
	// 这里刻意**不**检查 cache_hit_cost < cache_miss_cost。
	// 两者相等是合法且有意为之的配置（等价于"永远不扩展、每次重置"）——
	// 实测本项目真实配置就是这么设的。对着一个刻意的选择每次启动都告警，
	// 只会训练人忽略告警，反而让真正有用的那些被淹没。
	return nil
}

// validateProvider 校验 provider 在白名单内，报错时列出可选值。
func validateProvider(provider string) error {
	for _, supported := range supportedProviders {
		if provider == supported {
			return nil
		}
	}
	return fmt.Errorf("llm.provider 不支持 %q，目前只支持：%s",
		provider, strings.Join(supportedProviders, ", "))
}
