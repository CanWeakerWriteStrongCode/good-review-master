package config

import (
	"fmt"
	"strings"

	"good-review-master/logutil"

	"gopkg.in/yaml.v3"
)

// mcpSection MCP 工具服务配置（config.yaml 的 mcp 段）。
// 原 config.go 里那 70 行 parseMCP 整体搬到这里——字段、默认值、合法性判定现在同住一个文件。
type mcpSection struct {
	Enabled            bool            `yaml:"enabled"`
	ToolTimeoutSec     int             `yaml:"tool_timeout_sec"`
	MaxToolRounds      int             `yaml:"max_tool_rounds"`
	MaxToolResultRune  int             `yaml:"max_tool_result_rune"`
	RetryIntervalSec   int             `yaml:"retry_interval_sec"`
	BreakerFailures    int             `yaml:"breaker_failures"`
	BreakerCooldownSec int             `yaml:"breaker_cooldown_sec"`
	Servers            []mcpServerNode `yaml:"servers"`

	// accepted 是过滤后的可用服务，供 convert 取用。与 Servers 分开是刻意的：
	// Servers 是用户写了什么，accepted 是程序认了什么，后者才是运行时要用的
	accepted []MCPServerConf
}

// mcpServerNode config.yaml 里 mcp.servers 的单项
type mcpServerNode struct {
	Name      string            `yaml:"name"`
	Inject    *bool             `yaml:"inject"`
	Transport string            `yaml:"transport"`
	URL       string            `yaml:"url"`
	Token     string            `yaml:"token"` // 兼容旧位置；新配置放 secret.yaml
	Command   string            `yaml:"command"`
	Args      []string          `yaml:"args"`
	Env       map[string]string `yaml:"env"`
}

// retry interval 语义：不写（=0）走缺省 60s 自动重连；写负数表示禁用重连。
// 所以这里不能用 <=0 判缺省，必须精确判 ==0。
const (
	defaultMCPToolTimeoutSec    = 30
	defaultMCPMaxToolRounds     = 5
	defaultMCPMaxToolResultRune = 2000
	defaultMCPRetryIntervalSec  = 60

	// 熔断缺省：连续 5 次调用失败就打开，冷却 60 秒。比大模型那边松一档——
	// MCP 工具失败往往只影响某一次问答（模型可以改口直接作答），
	// 而大模型失败意味着这次回复彻底没了。
	defaultMCPBreakerFailures    = 5
	defaultMCPBreakerCooldownSec = 60
)

func (s *mcpSection) Name() string { return "mcp" }

func (s *mcpSection) Unmarshal(document *yaml.Node) error {
	return decodeSection(document, s.Name(), s)
}

// SetDefaults 填默认值，并顺带做条目级的归一化（去空白、小写 transport、补齐省略的 transport）。
//
// 归一化放在这一步而不是 Validate，是因为后面还会用它；Validate 只判定、不修改。
func (s *mcpSection) SetDefaults() {
	if s.ToolTimeoutSec <= 0 {
		s.ToolTimeoutSec = defaultMCPToolTimeoutSec
	}
	if s.MaxToolRounds <= 0 {
		s.MaxToolRounds = defaultMCPMaxToolRounds
	}
	if s.MaxToolResultRune <= 0 {
		s.MaxToolResultRune = defaultMCPMaxToolResultRune
	}
	if s.RetryIntervalSec == 0 {
		s.RetryIntervalSec = defaultMCPRetryIntervalSec
	}
	if s.BreakerFailures == 0 {
		s.BreakerFailures = defaultMCPBreakerFailures
	}
	if s.BreakerCooldownSec == 0 {
		s.BreakerCooldownSec = defaultMCPBreakerCooldownSec
	}

	s.accepted = nil
	if !s.Enabled {
		return
	}

	seen := make(map[string]bool, len(s.Servers))
	for _, node := range s.Servers {
		server := MCPServerConf{
			Name:      strings.TrimSpace(node.Name),
			Inject:    node.Inject,
			Transport: strings.ToLower(strings.TrimSpace(node.Transport)),
			URL:       strings.TrimSpace(node.URL),
			Token:     strings.TrimSpace(node.Token),
			Command:   strings.TrimSpace(node.Command),
			Args:      node.Args,
			Env:       node.Env,
		}
		switch {
		case server.Name == "":
			logutil.Warn("MCP 服务缺少 name，已跳过", "transport", server.Transport)
			continue
		case seen[server.Name]:
			logutil.Warn("MCP 服务 name 重复，已跳过后者", "name", server.Name)
			continue
		case server.Transport == "http", server.Transport == "sse":
			if server.URL == "" {
				logutil.Warn("MCP 服务 transport=http 但 url 为空，已跳过", "name", server.Name)
				continue
			}
		case server.Transport == "", server.Transport == "stdio":
			server.Transport = "stdio"
			if server.Command == "" {
				logutil.Warn("MCP 服务 transport=stdio 但 command 为空，已跳过", "name", server.Name)
				continue
			}
		default:
			logutil.Warn("MCP 服务 transport 无法识别，已跳过", "name", server.Name, "transport", server.Transport)
			continue
		}
		seen[server.Name] = true
		s.accepted = append(s.accepted, server)
	}
}

// Validate 只补充 SetDefaults 没能修掉的问题。
//
// 条目级的不完整（缺 name / stdio 缺 command / http 缺 url）**刻意不做成硬错误**：
// 那是"某个 MCP 服务配错了"，跳过它并告警即可，不该让整个机器人起不来——
// 这是改造前就有的语义（原 parseMCP 的注释写着"告警不致命"），保持不变。
func (s *mcpSection) Validate() error {
	if s.Enabled && len(s.accepted) == 0 {
		logutil.Warn("MCP 已启用但没有可用服务，对话不会注入工具")
	}
	if s.Enabled && s.MaxToolRounds <= 0 {
		return positiveInt("mcp.max_tool_rounds", s.MaxToolRounds, "单次对话最大工具调用轮数")
	}
	if s.BreakerFailures < -1 {
		return fmt.Errorf("mcp.breaker_failures 只能为 -1（关闭熔断）、0（用缺省 %d）或正整数，当前是 %d",
			defaultMCPBreakerFailures, s.BreakerFailures)
	}
	if s.BreakerCooldownSec < 0 {
		return fmt.Errorf("mcp.breaker_cooldown_sec 不能为负（当前 %d）：它是熔断后进入半开试探前的冷却秒数",
			s.BreakerCooldownSec)
	}
	return nil
}

// AcceptedServers 过滤后的可用服务列表（供 convert 用）。
func (s *mcpSection) AcceptedServers() []MCPServerConf {
	return s.accepted
}

// ServerTokens 返回 服务名 → token 的映射，供 Secret 域做迁移提示用。
func (s *mcpSection) ServerTokens() map[string]string {
	tokens := make(map[string]string, len(s.accepted))
	for _, server := range s.accepted {
		if server.Token != "" {
			tokens[server.Name] = server.Token
		}
	}
	return tokens
}
