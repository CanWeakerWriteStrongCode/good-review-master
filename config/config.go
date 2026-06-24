package config

import (
	"log/slog"
	"os"
	"path/filepath"
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

// resolveConfigPath 查找配置文件路径：优先工作目录，其次 exe 所在目录
func resolveConfigPath(filename string) string {
	for _, dir := range []string{".", exeDir()} {
		p := filepath.Join(dir, filename)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filename // 返回默认路径，后续 ReadFile 失败会报错
}

func exeDir() string {
	if exePath, err := os.Executable(); err == nil {
		return filepath.Dir(exePath)
	}
	return "."
}

func init() {
	path := resolveConfigPath("config.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		slog.Error("无法读取 config.yaml", "path", path, "err", err)
		os.Exit(1)
	}
	var cf configFile
	if err := yaml.Unmarshal(raw, &cf); err != nil {
		slog.Error("config.yaml 格式错误", "err", err)
		os.Exit(1)
	}

	NapCatHTTPAPI = cf.NapCat.HTTPAPI
	NapCatAccessToken = cf.NapCat.AccessToken
	BotQQ = cf.Bot.QQ
	AllowGroups = cf.Bot.AllowGroups
	MaxCacheMsg = cf.Runtime.MaxCacheMsg
	LLMTimeout = time.Duration(cf.Runtime.LLMTimeoutSec) * time.Second
	MaxMsgRune = cf.Runtime.MaxMsgRune
	PollInterval = time.Duration(cf.Runtime.PollIntervalSec) * time.Second

	LLMConfig = LLMConf{
		Provider:    cf.LLM.Provider,
		APIKey:      cf.LLM.APIKey,
		APIBase:     cf.LLM.APIBase,
		ModelName:   cf.LLM.ModelName,
		Temperature: cf.LLM.Temperature,
		TopP:        cf.LLM.TopP,
	}
}
