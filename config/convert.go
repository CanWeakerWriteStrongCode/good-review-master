package config

import (
	"time"

	"good-review-master/logutil"
)

// assemble 把各域（external 类型，带 yaml tag、负责解析）转成业务侧读的 Config
// （internal 类型，形状与改造前完全一致）。
//
// 这一层是 K8s external/internal 分离里真正可迁移的部分：解析格式与运行期表示分开，
// 转换只在这一个文件里发生。改造前这件事散在 LoadConfig 结尾那个 40 字段的返回字面量里，
// 加上一个逐字段镜像的 configFile，改一个字段要在三个地方同时改对。
//
// 密钥的优先级也在这里结算：环境变量 > secret.yaml > config.yaml 旧位置。
// （环境变量在 step 2 的 applyEnv 里就已经并进 secret 域，所以这里只需比两层。）
func assemble(sections *sections) *Config {
	secret := sections.Secret

	apiKey := resolveSecret("llm.api_key", secret.LLMAPIKey, sections.LLM.APIKey)
	webPassword := resolveSecret("runtime.web_password", secret.WebPassword, sections.Runtime.WebPassword)
	napCatToken := resolveSecret("napcat.access_token", secret.NapCatAccessToken, sections.NapCat.AccessToken)
	builtinToken := resolveSecret("runtime.mcp_builtin_token", secret.McpBuiltinToken, sections.Runtime.McpBuiltinToken)
	jwtSecret := resolveJWTSecret(secret.JWTSecret, sections.Runtime.JWTSecret, webPassword)

	servers := resolveMCPServers(sections.MCP.AcceptedServers(), secret)

	return &Config{
		NapCatHTTPAPI:     sections.NapCat.HTTPAPI,
		NapCatAccessToken: napCatToken,
		BotQQ:             sections.Bot.QQ,
		AllowGroups:       parseCommaList(sections.Bot.AllowGroups),
		MaxCacheMsg:       sections.Runtime.MaxCacheMsg,
		LLMSendCount:      sections.Runtime.LLMSendCount,
		LLMTimeout:        time.Duration(sections.Runtime.LLMTimeoutSec) * time.Second,
		MaxMsgRune:        sections.Runtime.MaxMsgRune,
		PollInterval:      time.Duration(sections.Runtime.PollIntervalSec) * time.Second,
		WebPort:           sections.Runtime.WebPort,
		WebUsername:       sections.Runtime.WebUsername,
		WebPassword:       webPassword,
		JWTSecret:         jwtSecret,
		McpBuiltinToken:   builtinToken,
		CorsOrigins:       sections.Runtime.CorsOriginList(),
		EnablePprof:       sections.Runtime.EnablePprof,
		ShutdownDelay:     time.Duration(sections.Runtime.ShutdownDelaySec) * time.Second,
		MetricsAddr:       sections.Runtime.MetricsAddr,
		OTLPEndpoint:      sections.Runtime.OTLPEndpoint,
		LLMConfig: LLMConf{
			Provider:         sections.LLM.Provider,
			APIKey:           apiKey,
			APIBase:          sections.LLM.APIBase,
			ModelName:        sections.LLM.ModelName,
			CacheHitCost:     sections.LLM.CacheHitCost,
			CacheMissCost:    sections.LLM.CacheMissCost,
			MaxContextTokens: sections.LLM.MaxContextTokens,
			ImageMax:         sections.LLM.ImageMax,
			Temperature:      sections.LLM.Temperature,
			TopP:             sections.LLM.TopP,

			RateLimitPerSec: sections.LLM.RateLimitPerSec,
			RateLimitBurst:  sections.LLM.RateLimitBurst,
			BreakerFailures: sections.LLM.BreakerFailures,
			BreakerCooldown: time.Duration(sections.LLM.BreakerCooldownSec) * time.Second,
		},
		MCPConfig: MCPConf{
			Enabled:           sections.MCP.Enabled,
			ToolTimeout:       time.Duration(sections.MCP.ToolTimeoutSec) * time.Second,
			MaxToolRounds:     sections.MCP.MaxToolRounds,
			MaxToolResultRune: sections.MCP.MaxToolResultRune,
			RetryInterval:     time.Duration(sections.MCP.RetryIntervalSec) * time.Second,
			BreakerFailures:   sections.MCP.BreakerFailures,
			BreakerCooldown:   time.Duration(sections.MCP.BreakerCooldownSec) * time.Second,
			Servers:           servers,
		},
	}
}

// resolveSecret 结算一个密钥的值：secret.yaml 优先，缺失则回退 config.yaml 的旧位置。
//
// 回退时打一条迁移提示，但**不自动改写用户文件**——配置归用户所有，
// 程序替他们搬动密钥是一件不该悄悄发生的事（何况还可能把密钥写进版本库）。
func resolveSecret(field, fromSecretFile, legacyValue string) string {
	if fromSecretFile != "" {
		return fromSecretFile
	}
	if legacyValue != "" {
		logutil.Warn("密钥仍写在 config.yaml 里，建议迁移到 secret.yaml（该文件应 0600 且不进版本库）",
			"字段", field, "位置", "config.yaml")
	}
	return legacyValue
}

// resolveJWTSecret 结算 JWT 签名密钥，比其他密钥多一层"回退到 web 密码"的兜底。
//
// 回退不是等价替换：密码一改，所有已签发的 token 立刻失效，而且两者共用同一份秘密。
// 所以回退时必须告警——这是改造前就有的语义，保留。
func resolveJWTSecret(fromSecretFile, legacyValue, webPassword string) string {
	if value := resolveSecret("runtime.jwt_secret", fromSecretFile, legacyValue); value != "" {
		return value
	}
	if webPassword != "" {
		logutil.Warn("未配置 jwt_secret，暂以 web_password 作为 JWT 签名密钥。" +
			"建议单独设置一个随机值：改密码会导致所有已登录会话立刻掉线")
	}
	return webPassword
}

// resolveMCPServers 给每个 MCP 服务补上 token：secret.yaml 的 mcp_server_tokens 优先，
// 回退到服务条目自己写的 token（旧位置）。
func resolveMCPServers(servers []MCPServerConf, secret *secretSection) []MCPServerConf {
	resolved := make([]MCPServerConf, 0, len(servers))
	for _, server := range servers {
		if token := secret.mcpToken(server.Name); token != "" {
			server.Token = token
		} else if server.Token != "" {
			logutil.Warn("MCP 服务的 token 仍写在 config.yaml 里，建议迁移到 secret.yaml",
				"服务", server.Name, "字段", "mcp.servers[].token")
		}
		resolved = append(resolved, server)
	}
	return resolved
}
