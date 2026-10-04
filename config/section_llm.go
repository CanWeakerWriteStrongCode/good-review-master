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
}

// 缺省值。cache_hit_cost / cache_miss_cost 是相对值，用于自动计算扩展阈值；
// 缺省按 deepseek 8/17 调价后的命中/未命中比 ≈ 1/30。
const (
	defaultCacheHitCost     = 0.033
	defaultCacheMissCost    = 1.0
	defaultMaxContextTokens = 50000
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
