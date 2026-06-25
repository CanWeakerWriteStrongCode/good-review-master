package config

import (
	"log/slog"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// ======================== 运行时常量（从 config.yaml 加载） ========================

var (
	NapCatHTTPAPI     string
	NapCatAccessToken string
	BotQQ             string
	BotNickname       string
	AllowGroups       string
	MaxCacheMsg       int
	LLMTimeout        time.Duration
	MaxMsgRune        int
	PollInterval      time.Duration
)

// LLMConf 大模型配置
type LLMConf struct {
	Provider    string
	APIKey      string
	APIBase     string
	ModelName   string
	Temperature float64
	TopP        float64
}

// LLMConfig 大模型配置实例
var LLMConfig LLMConf

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
		MaxCacheMsg     int `yaml:"max_cache_msg"`
		LLMTimeoutSec   int `yaml:"llm_timeout_sec"`
		MaxMsgRune      int `yaml:"max_msg_rune"`
		PollIntervalSec int `yaml:"poll_interval_sec"`
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

func init() {
	cfgPath := resolveConfigPath("config.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		// config.yaml 不存在，从内置模板创建
		destPath := writePath("config.yaml")
		if writeErr := os.WriteFile(destPath, configExampleTemplate, 0644); writeErr != nil {
			slog.Error("无法创建 config.yaml", "path", destPath, "err", writeErr)
			os.Exit(1)
		}
		slog.Info("已从内置模板创建 config.yaml，请编辑后重新运行", "path", destPath)
		os.Exit(0)
	}
	var cfg configFile
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		slog.Error("config.yaml 格式错误", "err", err)
		os.Exit(1)
	}

	NapCatHTTPAPI = cfg.NapCat.HTTPAPI
	NapCatAccessToken = cfg.NapCat.AccessToken
	BotQQ = cfg.Bot.QQ
	AllowGroups = cfg.Bot.AllowGroups
	MaxCacheMsg = cfg.Runtime.MaxCacheMsg
	LLMTimeout = time.Duration(cfg.Runtime.LLMTimeoutSec) * time.Second
	MaxMsgRune = cfg.Runtime.MaxMsgRune
	PollInterval = time.Duration(cfg.Runtime.PollIntervalSec) * time.Second

	LLMConfig = LLMConf{
		Provider:    cfg.LLM.Provider,
		APIKey:      cfg.LLM.APIKey,
		APIBase:     cfg.LLM.APIBase,
		ModelName:   cfg.LLM.ModelName,
		Temperature: cfg.LLM.Temperature,
		TopP:        cfg.LLM.TopP,
	}
}
