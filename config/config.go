package config

import (
	"good-review-master/logutil"
	"os"
	"strings"
	"time"

	"good-review-master/apppath"

	"gopkg.in/yaml.v3"
)

// Config 运行时配置（从 config.yaml 加载）
type Config struct {
	NapCatHTTPAPI     string
	NapCatAccessToken string
	BotQQ             string
	BotNickname       string // 运行时由 main 设置（GetLoginInfo 结果）
	AllowGroups       []string
	MaxCacheMsg       int
	LLMTimeout        time.Duration
	MaxMsgRune        int
	PollInterval      time.Duration
	WebPort           int    // Web 管理面板端口，<=0 禁用
	WebUsername       string // Web 管理面板登录账号
	WebPassword       string // Web 管理面板登录密码，空则不校验
	LLMConfig         LLMConf
}

// LLMConf 大模型配置
type LLMConf struct {
	Provider    string
	APIKey      string
	APIBase     string
	ModelName   string
	Temperature float64
	TopP        float64
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
		MaxCacheMsg     int    `yaml:"max_cache_msg"`
		LLMTimeoutSec   int    `yaml:"llm_timeout_sec"`
		MaxMsgRune      int    `yaml:"max_msg_rune"`
		PollIntervalSec int    `yaml:"poll_interval_sec"`
		WebPort         int    `yaml:"web_port"`
		WebUsername     string `yaml:"web_username"`
		WebPassword     string `yaml:"web_password"`
	} `yaml:"runtime"`
	LLM struct {
		Provider    string  `yaml:"provider"`
		APIKey      string  `yaml:"api_key"`
		APIBase     string  `yaml:"api_base"`
		ModelName   string  `yaml:"model_name"`
		Temperature float64 `yaml:"temperature"`
		TopP        float64 `yaml:"top_p"`
	} `yaml:"llm"`
}

// LoadConfig 从指定路径加载 config.yaml，若不存在则从内置模板创建
func LoadConfig(cfgPath string) (*Config, error) {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		// config.yaml 不存在，从内置模板创建
		destPath := apppath.WritePath("config.yaml")
		if writeErr := os.WriteFile(destPath, configExampleTemplate, 0644); writeErr != nil {
			logutil.Error("无法创建 config.yaml", "path", destPath, "err", writeErr)
			os.Exit(1)
		}
		logutil.Info("已从内置模板创建 config.yaml，请编辑后重新运行", "path", destPath)
		os.Exit(0)
	}
	var cfgFile configFile
	if err := yaml.Unmarshal(raw, &cfgFile); err != nil {
		logutil.Error("config.yaml 格式错误", "err", err)
		os.Exit(1)
	}

	allowGroups := parseAllowGroups(cfgFile.Bot.AllowGroups)

	return &Config{
		NapCatHTTPAPI:     cfgFile.NapCat.HTTPAPI,
		NapCatAccessToken: cfgFile.NapCat.AccessToken,
		BotQQ:             cfgFile.Bot.QQ,
		AllowGroups:       allowGroups,
		MaxCacheMsg:       cfgFile.Runtime.MaxCacheMsg,
		LLMTimeout:        time.Duration(cfgFile.Runtime.LLMTimeoutSec) * time.Second,
		MaxMsgRune:        cfgFile.Runtime.MaxMsgRune,
		PollInterval:      time.Duration(cfgFile.Runtime.PollIntervalSec) * time.Second,
		WebPort:           cfgFile.Runtime.WebPort,
		WebUsername:       cfgFile.Runtime.WebUsername,
		WebPassword:       cfgFile.Runtime.WebPassword,
		LLMConfig: LLMConf{
			Provider:    cfgFile.LLM.Provider,
			APIKey:      cfgFile.LLM.APIKey,
			APIBase:     cfgFile.LLM.APIBase,
			ModelName:   cfgFile.LLM.ModelName,
			Temperature: cfgFile.LLM.Temperature,
			TopP:        cfgFile.LLM.TopP,
		},
	}, nil
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
