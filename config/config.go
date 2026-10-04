package config

import (
	"fmt"
	"strings"
	"time"
)

// Config 运行时配置：业务代码读的那一份（internal 类型）。
//
// 它由各域（section_*.go，external 类型）经 convert.go 转换而来。
// **形状刻意与改造前保持一致**——分域只是加载期的组织方式，
// 消费者仍然写 cfg.BotQQ，一行都不用改。
// 这不只是图省事：mcpclient / router / web/server 的测试都直接构造这个结构体的字面量。
type Config struct {
	NapCatHTTPAPI     string
	NapCatAccessToken string
	BotQQ             string
	BotNickname       string // 运行时设置（GetLoginInfo 结果），不来自配置文件
	AllowGroups       []string
	MaxCacheMsg       int // 手动配置的环形缓冲条数上限（≈31×llm_send_count）
	LLMSendCount      int // 每次发送 LLM 的消息条数（重置窗口）
	LLMTimeout        time.Duration
	MaxMsgRune        int
	PollInterval      time.Duration
	WebPort           int           // Web 管理面板端口，<=0 禁用
	WebUsername       string        // Web 管理面板登录账号
	WebPassword       string        // Web 管理面板登录密码
	JWTSecret         string        // JWT 签名密钥；未配置时回退为 WebPassword
	McpBuiltinToken   string        // 内嵌 MCP 服务端鉴权 Bearer token（空=仅本机、不校验）
	CorsOrigins       []string      // 允许跨域访问管理面板的来源白名单；空=仅同源
	EnablePprof       bool          // 是否在已鉴权的 /api/debug/pprof/ 下开放 net/http/pprof
	ShutdownDelay     time.Duration // 优雅关闭时「先置 not-ready、等一会儿、再停监听」的等待时长；0=不等待
	MetricsAddr       string        // Prometheus /metrics 的独立监听地址；等于 config.MetricsAddrOff 表示关闭
	OTLPEndpoint      string        // OTLP/HTTP 追踪端点（如 http://localhost:4318）；空=关闭链路追踪
	LLMConfig         LLMConf
	MCPConfig         MCPConf
}

// MCPServerConf 单个 MCP 工具服务配置
type MCPServerConf struct {
	Name      string            // 服务名（必填且全局唯一；用于日志与跨服务工具重名消歧）
	Inject    *bool             // 是否把该服务的工具注入对话（不写默认注入；指针用于区分"没写"和"写了 false"）
	Transport string            // stdio（拉起本地命令）| http（streamable HTTP 远端）
	URL       string            // transport=http：服务端点
	Token     string            // transport=http：可选 Bearer Token（也可直接写在 url 查询串里）
	Command   string            // transport=stdio：启动命令，如 npx / uvx / python
	Args      []string          // transport=stdio：命令参数
	Env       map[string]string // transport=stdio：附加环境变量，叠加到当前进程环境之上
}

// ShouldInject 该服务的工具是否注入对话（yaml 里没写 inject 时默认注入）
func (s MCPServerConf) ShouldInject() bool {
	return s.Inject == nil || *s.Inject
}

// MCPConf MCP 工具服务总配置
type MCPConf struct {
	Enabled           bool            // 总开关，false 时一个服务都不连、对话也不带工具
	ToolTimeout       time.Duration   // 单个工具调用超时
	MaxToolRounds     int             // 单次对话最大工具调用轮数，达到后强制模型直接作答
	MaxToolResultRune int             // 单个工具返回结果最大字符数，超出截断（防撑爆上下文）
	RetryInterval     time.Duration   // 连接失败后的重连间隔，<=0 表示不自动重连
	BreakerFailures   int             // 单个服务的工具连续失败多少次后熔断；-1=关闭
	BreakerCooldown   time.Duration   // 熔断后进入半开试探前的冷却时长
	Servers           []MCPServerConf // 已通过校验的服务列表
}

// LLMConf 大模型配置
type LLMConf struct {
	Provider         string
	APIKey           string
	APIBase          string
	ModelName        string
	CacheHitCost     float64 // 缓存命中单价（相对值）
	CacheMissCost    float64 // 缓存未命中单价（相对值）
	MaxContextTokens int     // 单次发送大模型的上下文 token 上限（护栏，超限强制重置）
	ImageMax         int     // agent「看图」：单次回复最多实际查看(下载回传)的图片张数；>0 启用，0=关（纯文本）
	Temperature      float64
	TopP             float64

	RateLimitPerSec float64       // 出站全局限速（每秒允许的调用数）；0=不限速
	RateLimitBurst  int           // 令牌桶容量；0=按限速值向上取整、至少 1
	BreakerFailures int           // 连续失败多少次后熔断；-1=关闭熔断
	BreakerCooldown time.Duration // 熔断后进入半开试探前的冷却时长
}

// Validate 校验跨域的最终不变量——各域自己的规则在各自的 Validate 里，
// 这里只看"拼装完成后才成立"的那些。
//
// 放在 Config 而不是某个域上，是因为它横跨两个域：
// web_port 属于 runtime，而密码可能来自 secret.yaml，单个域看不到对方的字段。
func (cfg *Config) Validate() error {
	if cfg.WebPort > 0 && (cfg.WebUsername == "" || cfg.WebPassword == "") {
		return fmt.Errorf("web 管理面板已启用（runtime.web_port=%d）但未设置登录账号密码："+
			"请在 config.yaml 的 runtime 段配置 web_username，在 secret.yaml 配置 web_password"+
			"（本程序没有免密模式）", cfg.WebPort)
	}
	return nil
}

// HasGroup 检查群号是否在白名单中
func (cfg *Config) HasGroup(groupID string) bool {
	for _, group := range cfg.AllowGroups {
		if group == groupID {
			return true
		}
	}
	return false
}

// AllowGroupsStr 返回逗号分隔的群号字符串（用于日志输出）
func (cfg *Config) AllowGroupsStr() string {
	return strings.Join(cfg.AllowGroups, ",")
}

// MaskedAPIKey 返回脱敏后的 API Key（仅显示前4位和后4位）
func (cfg *Config) MaskedAPIKey() string {
	key := cfg.LLMConfig.APIKey
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", len(key)-8) + key[len(key)-4:]
}
