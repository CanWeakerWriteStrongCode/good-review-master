package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// runtimeSection 运行时参数与 Web 面板配置（config.yaml 的 runtime 段）
type runtimeSection struct {
	LLMSendCount     int    `yaml:"llm_send_count"`
	LLMTimeoutSec    int    `yaml:"llm_timeout_sec"`
	MaxMsgRune       int    `yaml:"max_msg_rune"`
	PollIntervalSec  int    `yaml:"poll_interval_sec"`
	WebPort          int    `yaml:"web_port"`
	WebUsername      string `yaml:"web_username"`
	WebPassword      string `yaml:"web_password"` // 兼容旧位置；新配置放 secret.yaml
	MaxCacheMsg      int    `yaml:"max_cache_msg"`
	McpBuiltinToken  string `yaml:"mcp_builtin_token"` // 兼容旧位置；新配置放 secret.yaml
	JWTSecret        string `yaml:"jwt_secret"`        // 兼容旧位置；新配置放 secret.yaml
	CorsOrigins      string `yaml:"cors_origins"`
	EnablePprof      bool   `yaml:"enable_pprof"`
	ShutdownDelaySec int    `yaml:"shutdown_delay_sec"`
}

// defaultLLMSendCount 与 llm_send_count 缺省值。
const defaultLLMSendCount = 20

// defaultCacheMsgMultiplier 环形缓冲条数上限相对 llm_send_count 的倍数。
// 取 31 是为了让扩展窗口能一路走到盈亏平衡点（见 docs/cache-cost-analysis.md）。
const defaultCacheMsgMultiplier = 31

func (s *runtimeSection) Name() string { return "runtime" }

func (s *runtimeSection) Unmarshal(document *yaml.Node) error {
	return decodeSection(document, s.Name(), s)
}

// SetDefaults 只处理"写了 0 或没写 = 用缺省值"这几个。
// 必须在 Validate 之前跑，否则缺省值会被 Validate 误判成非法值。
func (s *runtimeSection) SetDefaults() {
	if s.LLMSendCount <= 0 {
		s.LLMSendCount = defaultLLMSendCount
	}
	if s.MaxCacheMsg <= 0 {
		s.MaxCacheMsg = defaultCacheMsgMultiplier * s.LLMSendCount
	}
	s.WebUsername = strings.TrimSpace(s.WebUsername)
	s.WebPassword = strings.TrimSpace(s.WebPassword)
	s.JWTSecret = strings.TrimSpace(s.JWTSecret)
	s.McpBuiltinToken = strings.TrimSpace(s.McpBuiltinToken)
}

// Validate 判定本域是否"能工作"。
//
// 这些数字检查全部跑在 SetDefaults **之后**，所以像 llm_send_count 这种已经补过缺省值的
// 一定通过——它们不是重复劳动，而是把下游代码赖以成立的前提写成可执行的断言：
//
//   - PollInterval 进 time.NewTicker，<=0 会 **panic**，且是在轮询 goroutine 里 panic，
//     直接掀翻整个进程（这是改造前真实存在的隐患：漏写 poll_interval_sec 就会中招）
//   - LLMTimeout 进 context.WithTimeout，=0 得到一个立刻过期的 ctx，
//     表现为每一次大模型调用都失败在 "context deadline exceeded"
//   - MaxCacheMsg 进 make([]Message, n) 并随后被 buf[writeAt] 索引，=0 会 index out of range
//
// 阶段 3 引入热重载后这些断言才真正吃紧：那时配置不再只有启动时读一次这一个入口。
func (s *runtimeSection) Validate() error {
	if s.PollIntervalSec <= 0 {
		return positiveInt("runtime.poll_interval_sec", s.PollIntervalSec,
			"它直接进 time.NewTicker，为 0 会让轮询 goroutine panic 并拖垮整个进程")
	}
	if s.LLMTimeoutSec <= 0 {
		return positiveInt("runtime.llm_timeout_sec", s.LLMTimeoutSec,
			"它直接进 context.WithTimeout，为 0 会让每次大模型调用立刻超时失败")
	}
	if s.MaxMsgRune <= 0 {
		return positiveInt("runtime.max_msg_rune", s.MaxMsgRune,
			"单条消息的字符上限，为 0 等于不截断，超长消息会直接顶爆大模型上下文")
	}
	if s.LLMSendCount <= 0 {
		return positiveInt("runtime.llm_send_count", s.LLMSendCount, "缺省应为 20，走到这里说明 SetDefaults 没生效")
	}
	if s.MaxCacheMsg <= 0 {
		return positiveInt("runtime.max_cache_msg", s.MaxCacheMsg,
			"它进 make([]Message, n) 并被 buf[writeAt] 索引，为 0 会 index out of range")
	}
	if s.ShutdownDelaySec < 0 {
		return fmt.Errorf("runtime.shutdown_delay_sec 不能为负（当前 %d）：它表示优雅关闭前"+
			"等待流量摘除的秒数，0 表示不等待", s.ShutdownDelaySec)
	}
	return nil
}

// CorsOriginList 把逗号分隔的来源串转成切片（给 convert 用）。
func (s *runtimeSection) CorsOriginList() []string {
	return parseCommaList(s.CorsOrigins)
}
