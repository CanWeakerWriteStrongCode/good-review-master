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
// 消费者仍然写 cfg.BotNickname，一行都不用改。
// 这不只是图省事：mcpclient / router / web/server 的测试都直接构造这个结构体的字面量。
type Config struct {
	IMToken           string      // OneBot 12 接入共享 token（入站校验 + 出站 Bearer）
	IMAPIBase         string      // 所有账号默认的出站根地址
	IMAccounts        []IMAccount // 可选的按账号覆盖；api_base 为空表示沿用 IMAPIBase
	EchoSelfMessages  bool        // 实现端会把机器人自己发的消息回传为事件时置 true：本地不再重复记录回复
	BotNickname       string      // 运行时设置（get_self_info 结果），不来自配置文件
	AllowGroups       []string    // 群白名单；含 "*" 表示全部群
	WorkspaceAccounts []string    // 允许自动开通的工作空间 ID；含 "*" 表示全部

	// 主动插话（非 @ 时由大模型裁决）。全局一份，所有工作空间共用。
	ProactiveEnabled      bool
	ProactiveCooldown     time.Duration // 主动发言后该群的冷却
	ProactiveMaxStreak    int           // 同一话题最多连续主动发言次数
	ProactiveJudgeEvery   time.Duration // 两次判定的最小间隔
	ProactiveJudgeTimeout time.Duration // 单次判定超时（超时=不回复）
	ProactiveJudgeImages  int           // 每次判定最多附几张图；0=图片消息不判定
	MaxCacheMsg           int           // 手动配置的环形缓冲条数上限（≈31×llm_send_count）
	LLMSendCount          int           // 每次发送 LLM 的消息条数（重置窗口）
	LLMTimeout            time.Duration
	MaxMsgRune            int
	ReplySendInterval     time.Duration // 分句回复的发送间隔；>0 拆成最多 5 条逐条发，0=合成一条
	IMListenAddr          string        // AstrBot sidecar 推送事件的监听地址
	WebPort               int           // Web 管理面板端口，<=0 禁用
	WebUsername           string        // Web 管理面板登录账号
	WebPassword           string        // Web 管理面板登录密码
	JWTSecret             string        // JWT 签名密钥；未配置时回退为 WebPassword
	McpBuiltinToken       string        // 内嵌 MCP 服务端鉴权 Bearer token（空=仅本机、不校验）
	CorsOrigins           []string      // 允许跨域访问管理面板的来源白名单；空=仅同源
	EnablePprof           bool          // 是否在已鉴权的 /api/debug/pprof/ 下开放 net/http/pprof
	ShutdownDelay         time.Duration // 优雅关闭时「先置 not-ready、等一会儿、再停监听」的等待时长；0=不等待
	MetricsAddr           string        // Prometheus /metrics 的独立监听地址；等于 config.MetricsAddrOff 表示关闭
	OTLPEndpoint          string        // OTLP/HTTP 追踪端点（如 http://localhost:4318）；空=关闭链路追踪
	LLMConfig             LLMConf
	MCPConfig             MCPConf
}

// IMAccount 一个机器人账号的出站地址覆盖。
//
// ID 是工作空间 ID（platform_account）。只有多套实现端/多端口时才需要；
// 常见部署只配 im.api_base，账号 ID 由入站事件自动得到。
type IMAccount struct {
	ID      string
	APIBase string
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
	CacheHitCost     float64  // 缓存命中单价（相对值）
	CacheMissCost    float64  // 缓存未命中单价（相对值）
	MaxContextTokens int      // 单次发送大模型的上下文 token 上限（护栏，超限强制重置）
	ImageMax         int      // agent「看图」：单次回复最多实际查看(下载回传)的图片张数；>0 启用，0=关（纯文本）
	ImageLocalDirs   []string // view_image 允许读取的本地图片目录（QQ 官方图片是 AstrBot 本地临时文件）
	Temperature      float64
	TopP             float64

	ReplyMaxSentences    int // 最终回复的句数上限（模型出稿后再排一次版）；0=关闭排版
	ReplyRewriteMinRunes int // 原稿不足这么多字就不排版；0=不设下限

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

// HasGroup 检查群号是否在白名单中。白名单含 "*" 时放行所有群。
func (cfg *Config) HasGroup(groupID string) bool {
	for _, group := range cfg.AllowGroups {
		if group == "*" || group == groupID {
			return true
		}
	}
	return false
}

// HasWorkspaceAccount 判断某个工作空间 ID 是否允许自动开通。名单含 "*" 时放行全部。
//
// 只用于「新建」判定；已经有目录的工作空间由 workspace.Manager 直接加载，
// 不受这里影响——否则把 ID 从名单里删掉会让一个正在运行的账号突然失联。
func (cfg *Config) HasWorkspaceAccount(id string) bool {
	for _, account := range cfg.WorkspaceAccounts {
		if account == "*" || account == id {
			return true
		}
	}
	return false
}

// AllowsAllWorkspaceAccounts 返回名单是否包含通配符 "*"。
func (cfg *Config) AllowsAllWorkspaceAccounts() bool {
	for _, account := range cfg.WorkspaceAccounts {
		if account == "*" {
			return true
		}
	}
	return false
}

// WorkspaceAccountsStr 返回逗号分隔的工作空间白名单（用于日志输出）
func (cfg *Config) WorkspaceAccountsStr() string {
	return strings.Join(cfg.WorkspaceAccounts, ",")
}

// AllowsAllGroups 返回白名单是否包含通配符 "*"。
// 通配符无法枚举具体群，Web 群列表需要单独处理。
func (cfg *Config) AllowsAllGroups() bool {
	for _, group := range cfg.AllowGroups {
		if group == "*" {
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
