package config

import (
	"fmt"
	"good-review-master/logutil"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config 运行时配置（从 config.yaml 加载）
type Config struct {
	NapCatHTTPAPI     string
	NapCatAccessToken string
	BotQQ             string
	BotNickname       string // 运行时由 main 设置（GetLoginInfo 结果）
	AllowGroups       []string
	MaxCacheMsg       int // 手动配置的环形缓冲条数上限（≈31×llm_send_count）
	LLMSendCount      int // 每次发送 LLM 的消息条数（重置窗口）
	LLMTimeout        time.Duration
	MaxMsgRune        int
	PollInterval      time.Duration
	WebPort           int    // Web 管理面板端口，<=0 禁用
	WebUsername       string // Web 管理面板登录账号
	WebPassword       string // Web 管理面板登录密码，空则不校验
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
	Temperature      float64
	TopP             float64
}

type configFile struct {
	NapCat struct {
		HTTPAPI     string `yaml:"http_api"`
		AccessToken string `yaml:"access_token"`
	} `yaml:"napcat"`
	Bot struct {
		QQ          string `yaml:"qq"`
		AllowGroups string `yaml:"allow_groups"`
	} `yaml:"bot"`
	Runtime struct {
		LLMSendCount    int    `yaml:"llm_send_count"`
		LLMTimeoutSec   int    `yaml:"llm_timeout_sec"`
		MaxMsgRune      int    `yaml:"max_msg_rune"`
		PollIntervalSec int    `yaml:"poll_interval_sec"`
		WebPort         int    `yaml:"web_port"`
		WebUsername     string `yaml:"web_username"`
		WebPassword     string `yaml:"web_password"`
		MaxCacheMsg     int    `yaml:"max_cache_msg"`
	} `yaml:"runtime"`
	LLM struct {
		Provider         string  `yaml:"provider"`
		APIKey           string  `yaml:"api_key"`
		APIBase          string  `yaml:"api_base"`
		ModelName        string  `yaml:"model_name"`
		CacheHitCost     float64 `yaml:"cache_hit_cost"`
		CacheMissCost    float64 `yaml:"cache_miss_cost"`
		MaxContextTokens int     `yaml:"max_context_tokens"`
		Temperature      float64 `yaml:"temperature"`
		TopP             float64 `yaml:"top_p"`
	} `yaml:"llm"`
	MCP mcpFile `yaml:"mcp"`
}

// mcpFile config.yaml 的 mcp 段原始结构
type mcpFile struct {
	Enabled           bool            `yaml:"enabled"`
	ToolTimeoutSec    int             `yaml:"tool_timeout_sec"`
	MaxToolRounds     int             `yaml:"max_tool_rounds"`
	MaxToolResultRune int             `yaml:"max_tool_result_rune"`
	RetryIntervalSec  int             `yaml:"retry_interval_sec"`
	Servers           []mcpServerFile `yaml:"servers"`
}

// mcpServerFile config.yaml 的 mcp.servers 单项原始结构
type mcpServerFile struct {
	Name      string            `yaml:"name"`
	Inject    *bool             `yaml:"inject"`
	Transport string            `yaml:"transport"`
	URL       string            `yaml:"url"`
	Token     string            `yaml:"token"`
	Command   string            `yaml:"command"`
	Args      []string          `yaml:"args"`
	Env       map[string]string `yaml:"env"`
}

// LoadConfig 从指定路径加载 config.yaml，若不存在则从内置模板创建
func LoadConfig(cfgPath string) (*Config, error) {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("config.yaml 不存在，请先运行一次程序生成默认配置: %w", err)
	}
	var cfgFile configFile
	if err := yaml.Unmarshal(raw, &cfgFile); err != nil {
		logutil.Error("config.yaml 格式错误", "err", err)
		os.Exit(1)
	}

	allowGroups := parseAllowGroups(cfgFile.Bot.AllowGroups)

	// 缓存扩展配置：默认值 + 手动缓冲上限
	llmSendCount := cfgFile.Runtime.LLMSendCount
	if llmSendCount <= 0 {
		llmSendCount = 20
	}
	cacheHitCost := cfgFile.LLM.CacheHitCost
	if cacheHitCost <= 0 {
		cacheHitCost = 0.033
	}
	cacheMissCost := cfgFile.LLM.CacheMissCost
	if cacheMissCost <= 0 {
		cacheMissCost = 1.0
	}
	// 环形缓冲条数上限：手动配置，缺省按 ~31×llmSendCount（给扩展窗口留到盈亏平衡点）
	maxCacheMsg := cfgFile.Runtime.MaxCacheMsg
	if maxCacheMsg <= 0 {
		maxCacheMsg = 31 * llmSendCount
	}
	// 单次发送大模型的上下文 token 上限（护栏），超限强制重置
	maxContextTokens := cfgFile.LLM.MaxContextTokens
	if maxContextTokens <= 0 {
		maxContextTokens = 50000
	}

	mcpConf := parseMCP(&cfgFile.MCP)

	return &Config{
		NapCatHTTPAPI:     cfgFile.NapCat.HTTPAPI,
		NapCatAccessToken: cfgFile.NapCat.AccessToken,
		BotQQ:             cfgFile.Bot.QQ,
		AllowGroups:       allowGroups,
		MaxCacheMsg:       maxCacheMsg,
		LLMSendCount:      llmSendCount,
		LLMTimeout:        time.Duration(cfgFile.Runtime.LLMTimeoutSec) * time.Second,
		MaxMsgRune:        cfgFile.Runtime.MaxMsgRune,
		PollInterval:      time.Duration(cfgFile.Runtime.PollIntervalSec) * time.Second,
		WebPort:           cfgFile.Runtime.WebPort,
		WebUsername:       cfgFile.Runtime.WebUsername,
		WebPassword:       cfgFile.Runtime.WebPassword,
		LLMConfig: LLMConf{
			Provider:         cfgFile.LLM.Provider,
			APIKey:           cfgFile.LLM.APIKey,
			APIBase:          cfgFile.LLM.APIBase,
			ModelName:        cfgFile.LLM.ModelName,
			CacheHitCost:     cacheHitCost,
			CacheMissCost:    cacheMissCost,
			MaxContextTokens: maxContextTokens,
			Temperature:      cfgFile.LLM.Temperature,
			TopP:             cfgFile.LLM.TopP,
		},
		MCPConfig: mcpConf,
	}, nil
}

// parseMCP 解析 mcp 段：填默认值 + 校验并丢弃非法服务条目（告警不致命）。
// retry_interval_sec 语义：不写（=0）走缺省 60s 自动重连；写负数表示禁用重连。
func parseMCP(raw *mcpFile) MCPConf {
	toolTimeoutSec := raw.ToolTimeoutSec
	if toolTimeoutSec <= 0 {
		toolTimeoutSec = 30
	}
	maxToolRounds := raw.MaxToolRounds
	if maxToolRounds <= 0 {
		maxToolRounds = 5
	}
	maxToolResultRune := raw.MaxToolResultRune
	if maxToolResultRune <= 0 {
		maxToolResultRune = 2000
	}
	retrySec := raw.RetryIntervalSec
	if retrySec == 0 {
		retrySec = 60
	}

	conf := MCPConf{
		Enabled:           raw.Enabled,
		ToolTimeout:       time.Duration(toolTimeoutSec) * time.Second,
		MaxToolRounds:     maxToolRounds,
		MaxToolResultRune: maxToolResultRune,
		RetryInterval:     time.Duration(retrySec) * time.Second,
	}
	if !conf.Enabled {
		return conf
	}

	seen := make(map[string]bool, len(raw.Servers))
	for _, s := range raw.Servers {
		srv := MCPServerConf{
			Name:      strings.TrimSpace(s.Name),
			Inject:    s.Inject,
			Transport: strings.ToLower(strings.TrimSpace(s.Transport)),
			URL:       strings.TrimSpace(s.URL),
			Token:     s.Token,
			Command:   strings.TrimSpace(s.Command),
			Args:      s.Args,
			Env:       s.Env,
		}
		switch {
		case srv.Name == "":
			logutil.Warn("MCP 服务缺少 name，已跳过", "transport", srv.Transport)
			continue
		case seen[srv.Name]:
			logutil.Warn("MCP 服务 name 重复，已跳过后者", "name", srv.Name)
			continue
		case srv.Transport == "http", srv.Transport == "sse":
			if srv.URL == "" {
				logutil.Warn("MCP 服务 transport=http 但 url 为空，已跳过", "name", srv.Name)
				continue
			}
		case srv.Transport == "", srv.Transport == "stdio":
			srv.Transport = "stdio"
			if srv.Command == "" {
				logutil.Warn("MCP 服务 transport=stdio 但 command 为空，已跳过", "name", srv.Name)
				continue
			}
		default:
			logutil.Warn("MCP 服务 transport 无法识别，已跳过", "name", srv.Name, "transport", srv.Transport)
			continue
		}
		seen[srv.Name] = true
		conf.Servers = append(conf.Servers, srv)
	}
	if len(conf.Servers) == 0 {
		logutil.Warn("MCP 已启用但没有可用服务，对话不会注入工具")
	}
	return conf
}

// parseAllowGroups 解析逗号分隔的群号列表为字符串切片
func parseAllowGroups(raw string) []string {
	var groups []string
	for _, group := range strings.Split(raw, ",") {
		group = strings.TrimSpace(group)
		if group != "" {
			groups = append(groups, group)
		}
	}
	return groups
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
