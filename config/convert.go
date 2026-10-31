package config

import (
	"strings"
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
// 密钥只来自 secret.yaml 与环境变量；环境变量在 step 2 的 applyEnv 里
// 已经并进 secret 域，所以这里直接读即可。
func assemble(sections *sections) *Config {
	secret := sections.Secret

	apiKey := strings.TrimSpace(secret.LLMAPIKey)
	webPassword := strings.TrimSpace(secret.WebPassword)
	imToken := strings.TrimSpace(secret.IMToken)
	builtinToken := strings.TrimSpace(secret.McpBuiltinToken)
	jwtSecret := resolveJWTSecret(strings.TrimSpace(secret.JWTSecret), webPassword)

	servers := resolveMCPServers(sections.MCP.AcceptedServers(), secret)

	return &Config{
		IMToken:           imToken,
		IMAPIBase:         strings.TrimRight(strings.TrimSpace(sections.IM.APIBase), "/"),
		IMAccounts:        convertIMAccounts(sections.IM.Accounts),
		EchoSelfMessages:  sections.IM.EchoSelfMessages,
		BotNickname:       "",
		AllowGroups:       parseCommaList(sections.Bot.AllowGroups.String()),
		WorkspaceAccounts: convertWorkspaceAccounts([]string(sections.Bot.AllowWorkspaceAccounts)),

		ProactiveEnabled:      sections.Bot.ProactiveEnabled,
		ProactiveCooldown:     time.Duration(*sections.Bot.ProactiveCooldownSec) * time.Second,
		ProactiveMaxStreak:    *sections.Bot.ProactiveMaxStreak,
		ProactiveJudgeEvery:   time.Duration(*sections.Bot.ProactiveJudgeIntervalSec) * time.Second,
		ProactiveJudgeTimeout: time.Duration(*sections.Bot.ProactiveJudgeTimeoutSec) * time.Second,
		ProactiveJudgeImages:  *sections.Bot.ProactiveJudgeImages,
		MaxCacheMsg:           sections.Runtime.MaxCacheMsg,
		LLMSendCount:          sections.Runtime.LLMSendCount,
		LLMTimeout:            time.Duration(sections.Runtime.LLMTimeoutSec) * time.Second,
		MaxMsgRune:            sections.Runtime.MaxMsgRune,
		ReplySendInterval:     time.Duration(*sections.Runtime.ReplySendIntervalMs) * time.Millisecond,
		IMListenAddr:          sections.Runtime.IMListenAddr,
		WebPort:               sections.Runtime.WebPort,
		WebUsername:           sections.Runtime.WebUsername,
		WebPassword:           webPassword,
		JWTSecret:             jwtSecret,
		McpBuiltinToken:       builtinToken,
		CorsOrigins:           sections.Runtime.CorsOriginList(),
		EnablePprof:           sections.Runtime.EnablePprof,
		ShutdownDelay:         time.Duration(sections.Runtime.ShutdownDelaySec) * time.Second,
		MetricsAddr:           sections.Runtime.MetricsAddr,
		OTLPEndpoint:          sections.Runtime.OTLPEndpoint,
		LLMConfig: LLMConf{
			Provider:             sections.LLM.Provider,
			APIKey:               apiKey,
			APIBase:              sections.LLM.APIBase,
			ModelName:            sections.LLM.ModelName,
			CacheHitCost:         sections.LLM.CacheHitCost,
			CacheMissCost:        sections.LLM.CacheMissCost,
			MaxContextTokens:     sections.LLM.MaxContextTokens,
			ImageMax:             sections.LLM.ImageMax,
			ImageLocalDirs:       sections.LLM.ImageLocalDirs,
			Temperature:          sections.LLM.Temperature,
			TopP:                 sections.LLM.TopP,
			ReplyMaxSentences:    *sections.LLM.ReplyMaxSentences,
			ReplyRewriteMinRunes: *sections.LLM.ReplyRewriteMinRunes,

			ImageNoteMaxSide:     *sections.LLM.ImageNoteMaxSide,
			ImageNoteMaxPixels:   *sections.LLM.ImageNoteMaxPixels,
			ImageNoteTimeoutSec:  *sections.LLM.ImageNoteTimeoutSec,
			ImageNoteRetries:     *sections.LLM.ImageNoteRetries,
			ImageNoteConcurrency: *sections.LLM.ImageNoteConcurrency,

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
		Pipeline: sections.Pipeline.convert(),
		Assets:   sections.Assets.convert(),
	}
}

// convertIMAccounts 把 YAML 形态的出站账号转成运行期形态。
func convertIMAccounts(entries []imAccountSection) []IMAccount {
	out := make([]IMAccount, 0, len(entries))
	for _, entry := range entries {
		out = append(out, IMAccount{
			ID:      strings.TrimSpace(entry.ID),
			APIBase: strings.TrimRight(strings.TrimSpace(entry.APIBase), "/"),
		})
	}
	return out
}

// convertWorkspaceAccounts 去掉空项并保持声明顺序（面板/日志按它读）。
func convertWorkspaceAccounts(entries []string) []string {
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}
	return out
}

// resolveJWTSecret 结算 JWT 签名密钥，比其他密钥多一层"回退到 web 密码"的兜底。
//
// 回退不是等价替换：密码一改，所有已签发的 token 立刻失效，而且两者共用同一份秘密。
// 所以回退时必须告警——这是改造前就有的语义，保留。
func resolveJWTSecret(fromSecretFile, webPassword string) string {
	if fromSecretFile != "" {
		return fromSecretFile
	}
	if webPassword != "" {
		logutil.Warn("未配置 jwt_secret，暂以 web_password 作为 JWT 签名密钥。" +
			"建议单独设置一个随机值：改密码会导致所有已登录会话立刻掉线")
	}
	return webPassword
}

// resolveMCPServers 给每个 MCP 服务补上 token：只来自 secret.yaml 的 mcp_server_tokens。
func resolveMCPServers(servers []MCPServerConf, secret *secretSection) []MCPServerConf {
	resolved := make([]MCPServerConf, 0, len(servers))
	for _, server := range servers {
		server.Token = secret.mcpToken(server.Name)
		resolved = append(resolved, server)
	}
	return resolved
}
