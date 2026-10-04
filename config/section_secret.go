package config

import (
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// secretSection 密钥域。它跟其它域有两处结构性不同，都是刻意的：
//
//  1. **来源不是 config.yaml**，而是旁边的 secret.yaml + 环境变量。
//     所以它不在 scheme.go 的 registry() 里——硬塞进同一个循环会把
//     "到底读的哪份文档"藏起来，反而更难懂。
//  2. **secret.yaml 整份文件就是本域**，没有顶层键可找。所以它的 Unmarshal
//     直接解整份文档，不像别的域那样按 Name() 取自己那一段。
//
// 关于优先级：环境变量在 applyEnv 里**写进本结构体**，于是 convert 只需处理
// "secret > config.yaml 旧位置" 两层，环境变量天然排在最前——
// 三层优先级压成两层，少一处可能写错的地方。
type secretSection struct {
	LLMAPIKey         string            `yaml:"llm_api_key"`
	WebPassword       string            `yaml:"web_password"`
	JWTSecret         string            `yaml:"jwt_secret"`
	NapCatAccessToken string            `yaml:"napcat_access_token"`
	McpBuiltinToken   string            `yaml:"mcp_builtin_token"`
	McpServerTokens   map[string]string `yaml:"mcp_server_tokens"`
}

// 环境变量名。只覆盖这三个全局值；MCP 每个服务的 token 只走文件
// （按服务名做环境变量太容易撞名，收益也不大）。
const (
	envLLMAPIKey   = "GOOD_REVIEW_LLM_API_KEY"
	envWebPassword = "GOOD_REVIEW_WEB_PASSWORD"
	envNapCatToken = "GOOD_REVIEW_NAPCAT_TOKEN"
)

func (s *secretSection) Name() string { return "secret" }

// Unmarshal 解整份 secret.yaml（见上面第 2 点的说明）。
func (s *secretSection) Unmarshal(document *yaml.Node) error {
	if document == nil {
		return nil
	}
	return document.Decode(s)
}

// applyEnv 应用环境变量覆盖。必须在 Unmarshal 之后、convert 之前调用。
func (s *secretSection) applyEnv() {
	if value := strings.TrimSpace(os.Getenv(envLLMAPIKey)); value != "" {
		s.LLMAPIKey = value
	}
	if value := strings.TrimSpace(os.Getenv(envWebPassword)); value != "" {
		s.WebPassword = value
	}
	if value := strings.TrimSpace(os.Getenv(envNapCatToken)); value != "" {
		s.NapCatAccessToken = value
	}
}

func (s *secretSection) SetDefaults() {
	s.LLMAPIKey = strings.TrimSpace(s.LLMAPIKey)
	s.WebPassword = strings.TrimSpace(s.WebPassword)
	s.JWTSecret = strings.TrimSpace(s.JWTSecret)
	s.NapCatAccessToken = strings.TrimSpace(s.NapCatAccessToken)
	s.McpBuiltinToken = strings.TrimSpace(s.McpBuiltinToken)
	for name, token := range s.McpServerTokens {
		s.McpServerTokens[name] = strings.TrimSpace(token)
	}
}

// Validate 不做硬判定。
//
// 原因：密钥缺失是**允许**的状态，缺了由 convert 回退到 config.yaml 旧位置或留空。
// 具体哪些缺失会致命（比如开了 web_port 却没有密码）跨了 runtime 与 secret 两个域，
// 那类规则统一放在 assemble 之后的 Config.Validate 里——它能看到拼装完成的整体。
func (s *secretSection) Validate() error { return nil }

// mcpToken 取某个 MCP 服务的 token（没有则空串）。
func (s *secretSection) mcpToken(serverName string) string {
	if s.McpServerTokens == nil {
		return ""
	}
	return s.McpServerTokens[serverName]
}
