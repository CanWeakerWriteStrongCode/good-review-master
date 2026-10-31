package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"good-review-master/logutil"
)

// TestMain 把 cwd 切到临时目录：logutil.SetupLogger 会在 apppath.ExeDir()（优先 cwd）
// 下建 log/，而 go test 的 cwd 就是包目录——不隔离就会在源码树里留下 config/log/。
// 同时必须真的初始化 logger：本包的若干路径（JWT 回退提示等）会打 WARN，
// 而 logutil 未初始化时是 nil 指针，一调就 panic。
func TestMain(m *testing.M) {
	original, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	temp, err := os.MkdirTemp("", "config-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(temp); err != nil {
		panic(err)
	}
	logutil.SetupLogger()

	code := m.Run()

	logutil.Close()
	_ = os.Chdir(original)
	_ = os.RemoveAll(temp)
	os.Exit(code)
}

// baseConfig 一份能通过全部校验的最小配置（不含任何密钥）。
const baseConfig = `
im:
  api_base: "http://127.0.0.1:6199"
bot:
  allow_groups: "10001,10002"
  allow_workspace_accounts: ["qq_official_bot_1906xxxx", "qq_official_bot_1904xxxx"]
runtime:
  llm_send_count: 3
  llm_timeout_sec: 20
  max_msg_rune: 500
  web_port: 9090
  web_username: "admin"
llm:
  provider: "openai"
  api_base: "https://api.example.com"
  model_name: "test-model"
  temperature: 1.0
  top_p: 0.95
`

// baseSecret 测试共用的密钥文件：web 面板必须有密码，其余用例大多不关心具体值。
const baseSecret = `
llm_api_key: "sk-test"
web_password: "123456"
im_token: "im-token"
`

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入 %s 失败：%v", name, err)
	}
	return path
}

func loadWith(t *testing.T, configYAML, secretYAML string) (*Config, error) {
	t.Helper()
	if secretYAML == "" {
		secretYAML = baseSecret
	}
	sources := Sources{
		Config: writeFile(t, "config.yaml", configYAML),
		Secret: writeFile(t, "secret.yaml", secretYAML),
	}
	return Load(sources)
}

func TestLoad基本配置(t *testing.T) {
	cfg, err := loadWith(t, baseConfig, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.BotNickname != "" {
		t.Errorf("BotNickname 应来自运行时，不应被配置文件填充：%q", cfg.BotNickname)
	}
	if len(cfg.AllowGroups) != 2 || cfg.AllowGroups[0] != "10001" {
		t.Errorf("AllowGroups = %v", cfg.AllowGroups)
	}
	if len(cfg.WorkspaceAccounts) != 2 || cfg.WorkspaceAccounts[0] != "qq_official_bot_1906xxxx" {
		t.Errorf("WorkspaceAccounts = %v", cfg.WorkspaceAccounts)
	}
	if cfg.IMToken != "im-token" {
		t.Errorf("IMToken = %q", cfg.IMToken)
	}
	if cfg.IMAPIBase != "http://127.0.0.1:6199" {
		t.Errorf("IMAPIBase = %q", cfg.IMAPIBase)
	}
	if cfg.LLMConfig.ModelName != "test-model" {
		t.Errorf("ModelName = %q", cfg.LLMConfig.ModelName)
	}
}

// TestSetDefaults在Validate之前跑 锁住四步曲的顺序。
// 缺省值必须在校验前补上，否则"没写 llm_send_count"会被校验当成非法值直接拒绝启动。
func TestSetDefaults在Validate之前跑(t *testing.T) {
	cfg, err := loadWith(t, `
bot:
  allow_groups: "1"
runtime:
  llm_timeout_sec: 20
  max_msg_rune: 500
llm:
  model_name: "m"
`, "")
	if err != nil {
		t.Fatalf("省略了带缺省值的字段就不该加载失败：%v", err)
	}
	if cfg.LLMSendCount != defaultLLMSendCount {
		t.Errorf("llm_send_count 缺省应为 %d，实际 %d", defaultLLMSendCount, cfg.LLMSendCount)
	}
	// max_cache_msg 缺省依赖 llm_send_count 的缺省值，两者有先后关系
	if want := defaultCacheMsgMultiplier * defaultLLMSendCount; cfg.MaxCacheMsg != want {
		t.Errorf("max_cache_msg 缺省应为 %d，实际 %d", want, cfg.MaxCacheMsg)
	}
}

func Test密钥来自secret文件(t *testing.T) {
	cfg, err := loadWith(t, baseConfig, `
llm_api_key: "sk-from-secret-file"
web_password: "pw-from-secret-file"
im_token: "im-from-secret-file"
`)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.LLMConfig.APIKey != "sk-from-secret-file" {
		t.Errorf("APIKey = %q", cfg.LLMConfig.APIKey)
	}
	if cfg.WebPassword != "pw-from-secret-file" {
		t.Errorf("WebPassword = %q", cfg.WebPassword)
	}
	if cfg.IMToken != "im-from-secret-file" {
		t.Errorf("IMToken = %q", cfg.IMToken)
	}
}

func TestIM账号可覆盖全局地址(t *testing.T) {
	configYAML := strings.Replace(baseConfig, `  api_base: "http://127.0.0.1:6199"`,
		`  api_base: "http://127.0.0.1:6199"
  accounts:
    - id: "qq_official_bot_1906xxxx"
    - id: "qq_official_bot_1904xxxx"
      api_base: "http://127.0.0.1:6200"`, 1)
	cfg, err := loadWith(t, configYAML, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if len(cfg.IMAccounts) != 2 {
		t.Fatalf("应有 2 条覆盖，实际 %+v", cfg.IMAccounts)
	}
	if cfg.IMAccounts[0].APIBase != "" {
		t.Errorf("第一条未写 api_base，应留空表示沿用全局，实际 %q", cfg.IMAccounts[0].APIBase)
	}
	if cfg.IMAccounts[1].APIBase != "http://127.0.0.1:6200" {
		t.Errorf("第二条应使用自己的地址，实际 %q", cfg.IMAccounts[1].APIBase)
	}
}

func Test回传自身消息开关(t *testing.T) {
	cfg, err := loadWith(t, baseConfig, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.EchoSelfMessages {
		t.Fatal("im.echo_self_messages 缺省应为 false")
	}

	enabled := strings.Replace(baseConfig, `  api_base: "http://127.0.0.1:6199"`,
		`  api_base: "http://127.0.0.1:6199"
  echo_self_messages: true`, 1)
	cfg, err = loadWith(t, enabled, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if !cfg.EchoSelfMessages {
		t.Fatal("显式 true 应生效")
	}
}

func Test环境变量优先于secret(t *testing.T) {
	t.Setenv(envLLMAPIKey, "sk-from-env")
	t.Setenv(envWebPassword, "pw-from-env")

	cfg, err := loadWith(t, baseConfig, `
llm_api_key: "sk-from-secret-file"
web_password: "pw-from-secret-file"
`)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.LLMConfig.APIKey != "sk-from-env" {
		t.Errorf("环境变量应最优先，实际 APIKey = %q", cfg.LLMConfig.APIKey)
	}
	if cfg.WebPassword != "pw-from-env" {
		t.Errorf("环境变量应最优先，实际 WebPassword = %q", cfg.WebPassword)
	}
}

func TestJWT回退到web密码(t *testing.T) {
	cfg, err := loadWith(t, baseConfig, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.JWTSecret != cfg.WebPassword {
		t.Errorf("未配 jwt_secret 时应回退 web 密码，实际 JWTSecret = %q", cfg.JWTSecret)
	}

	cfg, err = loadWith(t, baseConfig, "web_password: \"123456\"\njwt_secret: \"dedicated\"\n")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.JWTSecret != "dedicated" {
		t.Errorf("配了 jwt_secret 就不该回退，实际 %q", cfg.JWTSecret)
	}
}

func TestMCPToken来自secret(t *testing.T) {
	configYAML := baseConfig + `
mcp:
  enabled: true
  servers:
    - name: "svc"
      transport: "http"
      url: "https://example.com/mcp"
`
	cfg, err := loadWith(t, configYAML, "web_password: \"123456\"\nmcp_server_tokens:\n  svc: \"token-in-secret\"\n")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if len(cfg.MCPConfig.Servers) != 1 {
		t.Fatalf("应有 1 个服务，实际 %d", len(cfg.MCPConfig.Servers))
	}
	if got := cfg.MCPConfig.Servers[0].Token; got != "token-in-secret" {
		t.Errorf("mcp_server_tokens 应生效，实际 %q", got)
	}
}

// TestMCPSkip非法条目不致命 锁住改造前就有的语义：
// 某个 MCP 服务配错只跳过它并告警，不该让整个机器人起不来。
func TestMCPSkip非法条目不致命(t *testing.T) {
	configYAML := baseConfig + `
mcp:
  enabled: true
  servers:
    - name: "no-command"
      transport: "stdio"
    - name: "no-url"
      transport: "http"
    - name: "bad-transport"
      transport: "carrier-pigeon"
      url: "https://example.com"
    - name: "good"
      transport: "http"
      url: "https://example.com/mcp"
    - name: "good"
      transport: "http"
      url: "https://duplicate.example.com/mcp"
`
	cfg, err := loadWith(t, configYAML, "")
	if err != nil {
		t.Fatalf("非法服务条目不该导致加载失败：%v", err)
	}
	if len(cfg.MCPConfig.Servers) != 1 {
		t.Fatalf("只应留下 1 个合法服务，实际 %d：%v", len(cfg.MCPConfig.Servers), cfg.MCPConfig.Servers)
	}
	if cfg.MCPConfig.Servers[0].Name != "good" {
		t.Errorf("留下的应是 good，实际 %q", cfg.MCPConfig.Servers[0].Name)
	}
}

func TestMCPRetryInterval负数表示禁用重连(t *testing.T) {
	configYAML := baseConfig + `
mcp:
  enabled: true
  retry_interval_sec: -1
  servers:
    - name: "svc"
      transport: "http"
      url: "https://example.com/mcp"
`
	cfg, err := loadWith(t, configYAML, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	// -1 必须原样保留（<=0 表示不自动重连），不能被当成"没写"而补成 60s
	if cfg.MCPConfig.RetryInterval >= 0 {
		t.Errorf("retry_interval_sec: -1 应保留为负，实际 %v", cfg.MCPConfig.RetryInterval)
	}
}

// ===== 校验 =====

func TestValidate报错要指明字段与原因(t *testing.T) {
	cases := []struct {
		name        string
		configYAML  string
		secretYAML  string
		wantContain string
	}{
		{
			name: "llm_timeout_sec 为 0 会让调用立刻超时",
			configYAML: strings.Replace(baseConfig,
				"  llm_timeout_sec: 20", "  llm_timeout_sec: 0", 1),
			wantContain: "llm_timeout_sec",
		},
		{
			name: "max_msg_rune 为 0 等于不截断",
			configYAML: strings.Replace(baseConfig,
				"  max_msg_rune: 500", "  max_msg_rune: 0", 1),
			wantContain: "max_msg_rune",
		},
		{
			name: "provider 不在白名单",
			configYAML: strings.Replace(baseConfig,
				`  provider: "openai"`, `  provider: "anthropic"`, 1),
			wantContain: "provider",
		},
		{
			name: "im.api_base 地址没有 scheme",
			configYAML: strings.Replace(baseConfig,
				`  api_base: "http://127.0.0.1:6199"`, `  api_base: "127.0.0.1:6199"`, 1),
			wantContain: "api_base",
		},
		{
			name:        "开了 web_port 却没有密码",
			configYAML:  baseConfig,
			secretYAML:  `im_token: "x"`,
			wantContain: "web",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := loadWith(t, testCase.configYAML, testCase.secretYAML)
			if err == nil {
				t.Fatal("应报错但通过了")
			}
			// 错误是给"改 YAML 的人"看的：必须点名字段，不能是笼统的"配置非法"
			if !strings.Contains(err.Error(), testCase.wantContain) {
				t.Fatalf("错误信息里应出现字段名 %q，实际：%v", testCase.wantContain, err)
			}
		})
	}
}

func TestValidate负的shutdown_delay被拒(t *testing.T) {
	configYAML := strings.Replace(baseConfig,
		"  web_username: \"admin\"", "  shutdown_delay_sec: -5\n  web_username: \"admin\"", 1)
	if _, err := loadWith(t, configYAML, ""); err == nil {
		t.Fatal("shutdown_delay_sec 为负应被拒绝")
	}
}

func TestValidate缺少config文件(t *testing.T) {
	_, err := Load(Sources{Config: filepath.Join(t.TempDir(), "不存在.yaml")})
	if err == nil {
		t.Fatal("config.yaml 不存在时应报错")
	}
	if !strings.Contains(err.Error(), "不存在") {
		t.Errorf("错误信息应说清是文件不存在，实际：%v", err)
	}
}

// TestLoad幂等 是阶段 3 的前置条件：informer 会反复调 Load，
// 两次加载必须得到完全相同的配置，否则每次重载都会漂移一点。
func TestLoad幂等(t *testing.T) {
	path := writeFile(t, "config.yaml", baseConfig)
	secretPath := writeFile(t, "secret.yaml", baseSecret)
	sources := Sources{Config: path, Secret: secretPath}
	first, err := Load(sources)
	if err != nil {
		t.Fatalf("首次加载失败：%v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := Load(sources)
		if err != nil {
			t.Fatalf("第 %d 次加载失败：%v", i+2, err)
		}
		if !configsEqual(first, again) {
			t.Fatalf("第 %d 次加载结果与首次不同（Load 不幂等）", i+2)
		}
	}
}

// TestLoad不创建文件 Load 必须无副作用——建模板只能发生在 InitDefaultFiles。
// 否则 informer 每次重载都可能往用户目录里写东西。
func TestLoad不创建文件(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(baseConfig), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	secretPath := filepath.Join(dir, "secret.yaml")
	if err := os.WriteFile(secretPath, []byte(baseSecret), 0o600); err != nil {
		t.Fatalf("写入失败：%v", err)
	}
	before := dirEntries(t, dir)

	if _, err := Load(Sources{Config: path, Secret: secretPath}); err != nil {
		t.Fatalf("加载失败：%v", err)
	}

	after := dirEntries(t, dir)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("Load 改变了目录内容：\n前 %v\n后 %v", before, after)
	}
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读目录失败：%v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// configsEqual 逐字段比较两份配置。用 == 会漏掉切片与 map 的内部差异。
func configsEqual(a, b *Config) bool {
	if a.IMToken != b.IMToken || a.IMAPIBase != b.IMAPIBase ||
		a.MaxCacheMsg != b.MaxCacheMsg ||
		a.LLMSendCount != b.LLMSendCount || a.LLMTimeout != b.LLMTimeout ||
		a.MaxMsgRune != b.MaxMsgRune ||
		a.WebPort != b.WebPort || a.WebUsername != b.WebUsername ||
		a.WebPassword != b.WebPassword || a.JWTSecret != b.JWTSecret ||
		a.McpBuiltinToken != b.McpBuiltinToken || a.EnablePprof != b.EnablePprof ||
		a.ShutdownDelay != b.ShutdownDelay {
		return false
	}
	if strings.Join(a.AllowGroups, ",") != strings.Join(b.AllowGroups, ",") {
		return false
	}
	if strings.Join(a.WorkspaceAccounts, ",") != strings.Join(b.WorkspaceAccounts, ",") {
		return false
	}
	if a.ProactiveEnabled != b.ProactiveEnabled ||
		a.ProactiveCooldown != b.ProactiveCooldown ||
		a.ProactiveMaxStreak != b.ProactiveMaxStreak ||
		a.ProactiveJudgeEvery != b.ProactiveJudgeEvery ||
		a.ProactiveJudgeTimeout != b.ProactiveJudgeTimeout ||
		a.ProactiveJudgeImages != b.ProactiveJudgeImages {
		return false
	}
	if !reflect.DeepEqual(a.IMAccounts, b.IMAccounts) {
		return false
	}
	if strings.Join(a.CorsOrigins, ",") != strings.Join(b.CorsOrigins, ",") {
		return false
	}
	if !reflect.DeepEqual(a.LLMConfig, b.LLMConfig) {
		return false
	}
	if len(a.MCPConfig.Servers) != len(b.MCPConfig.Servers) {
		return false
	}
	for i := range a.MCPConfig.Servers {
		left, right := a.MCPConfig.Servers[i], b.MCPConfig.Servers[i]
		if left.Name != right.Name || left.Transport != right.Transport ||
			left.URL != right.URL || left.Token != right.Token || left.Command != right.Command ||
			left.ShouldInject() != right.ShouldInject() {
			return false
		}
	}
	return true
}

// TestProactive默认值 没写主动插话配置时，默认关闭且各阈值取文档里的缺省值。
func TestProactive默认值(t *testing.T) {
	cfg, err := loadWith(t, baseConfig, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.ProactiveEnabled {
		t.Fatal("proactive_enabled 未配置时应默认关闭")
	}
	if cfg.ProactiveCooldown != 90*time.Second ||
		cfg.ProactiveMaxStreak != 5 || cfg.ProactiveJudgeEvery != 20*time.Second ||
		cfg.ProactiveJudgeTimeout != 20*time.Second || cfg.ProactiveJudgeImages != 1 {
		t.Fatalf("主动插话默认值不对：cooldown=%v streak=%d every=%v timeout=%v images=%d",
			cfg.ProactiveCooldown, cfg.ProactiveMaxStreak,
			cfg.ProactiveJudgeEvery, cfg.ProactiveJudgeTimeout, cfg.ProactiveJudgeImages)
	}
}

// TestProactive显式配置 显式写的值要原样进 Config，images=0 表示不判图（不能被默认值覆盖）。
func TestProactive显式配置(t *testing.T) {
	yaml := strings.Replace(baseConfig,
		`allow_workspace_accounts: ["qq_official_bot_1906xxxx", "qq_official_bot_1904xxxx"]`,
		`allow_workspace_accounts: ["qq_official_bot_1906xxxx", "qq_official_bot_1904xxxx"]
  proactive_enabled: true
  proactive_cooldown_sec: 30
  proactive_max_streak: 2
  proactive_judge_interval_sec: 5
  proactive_judge_timeout_sec: 3
  proactive_judge_images: 0`, 1)
	cfg, err := loadWith(t, yaml, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if !cfg.ProactiveEnabled ||
		cfg.ProactiveCooldown != 30*time.Second || cfg.ProactiveMaxStreak != 2 ||
		cfg.ProactiveJudgeEvery != 5*time.Second || cfg.ProactiveJudgeTimeout != 3*time.Second ||
		cfg.ProactiveJudgeImages != 0 {
		t.Fatalf("显式配置未生效：%+v", cfg)
	}
}

// TestProactive显式零生效 0 是有效配置（不冷却/无判定间隔/不主动发言/沿用 llm 超时），
// 只有删掉那一行才会用默认值；负数非法。
func TestProactive显式零生效(t *testing.T) {
	yaml := strings.Replace(baseConfig,
		`allow_workspace_accounts: ["qq_official_bot_1906xxxx", "qq_official_bot_1904xxxx"]`,
		`allow_workspace_accounts: ["qq_official_bot_1906xxxx", "qq_official_bot_1904xxxx"]
  proactive_enabled: true
  proactive_cooldown_sec: 0
  proactive_max_streak: 0
  proactive_judge_interval_sec: 0
  proactive_judge_timeout_sec: 0
  proactive_judge_images: 0`, 1)
	cfg, err := loadWith(t, yaml, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.ProactiveCooldown != 0 || cfg.ProactiveMaxStreak != 0 ||
		cfg.ProactiveJudgeEvery != 0 || cfg.ProactiveJudgeTimeout != 0 ||
		cfg.ProactiveJudgeImages != 0 {
		t.Fatalf("显式 0 被默认值覆盖了：%+v", cfg)
	}

	negative := strings.Replace(baseConfig,
		`allow_workspace_accounts: ["qq_official_bot_1906xxxx", "qq_official_bot_1904xxxx"]`,
		`allow_workspace_accounts: ["qq_official_bot_1906xxxx", "qq_official_bot_1904xxxx"]
  proactive_cooldown_sec: -1`, 1)
	if _, err := loadWith(t, negative, ""); err == nil {
		t.Fatal("负数应被拒绝")
	}
}

// TestAllowGroups兼容字符串与列表 allow_groups 两种 YAML 写法都要能加载：
// 历史逗号字符串不能失效，列表形态要按顺序保留、忽略空白项。
func TestAllowGroups兼容字符串与列表(t *testing.T) {
	cases := []struct {
		name   string
		groups string
		want   []string
	}{
		{
			name:   "逗号分隔字符串",
			groups: "  allow_groups: \"10001, 10002\"\n",
			want:   []string{"10001", "10002"},
		},
		{
			name:   "YAML 列表",
			groups: "  allow_groups:\n    - \"10001\"\n    - \"10002\"\n",
			want:   []string{"10001", "10002"},
		},
		{
			name:   "列表带注释与空项",
			groups: "  allow_groups:\n    # 群 A\n    - \"10001\"\n    - \"\"\n    - \"10002\"\n",
			want:   []string{"10001", "10002"},
		},
		{
			name:   "空值",
			groups: "  allow_groups:\n",
			want:   nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, "bot:\n"+tc.groups+`runtime:
  llm_timeout_sec: 20
  max_msg_rune: 500
llm:
  model_name: "m"
`, "")
			if err != nil {
				t.Fatalf("加载失败：%v", err)
			}
			if !reflect.DeepEqual(cfg.AllowGroups, tc.want) {
				t.Fatalf("AllowGroups = %v，期望 %v", cfg.AllowGroups, tc.want)
			}
		})
	}
}

// TestAllowWorkspaceAccounts兼容字符串与列表 与 allow_groups 一样：
// 逗号分隔字符串和 YAML 列表都要能加载，并保持声明顺序。
func TestAllowWorkspaceAccounts兼容字符串与列表(t *testing.T) {
	cases := []struct {
		name     string
		accounts string
		want     []string
	}{
		{
			name:     "逗号分隔字符串",
			accounts: "  allow_workspace_accounts: \"qq_official_bot_a1, qq_official_bot_a2\"\n",
			want:     []string{"qq_official_bot_a1", "qq_official_bot_a2"},
		},
		{
			name:     "YAML 列表",
			accounts: "  allow_workspace_accounts:\n    - \"qq_official_bot_a1\"\n    - \"qq_official_bot_a2\"\n",
			want:     []string{"qq_official_bot_a1", "qq_official_bot_a2"},
		},
		{
			name:     "列表带注释与空项",
			accounts: "  allow_workspace_accounts:\n    # 账号 A\n    - \"qq_official_bot_a1\"\n    - \"\"\n    - \"qq_official_bot_a2\"\n",
			want:     []string{"qq_official_bot_a1", "qq_official_bot_a2"},
		},
		{
			name:     "空值",
			accounts: "  allow_workspace_accounts:\n",
			want:     []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadWith(t, "bot:\n"+tc.accounts+`runtime:
  llm_timeout_sec: 20
  max_msg_rune: 500
llm:
  model_name: "m"
`, "")
			if err != nil {
				t.Fatalf("加载失败：%v", err)
			}
			if !reflect.DeepEqual(cfg.WorkspaceAccounts, tc.want) {
				t.Fatalf("WorkspaceAccounts = %v，期望 %v", cfg.WorkspaceAccounts, tc.want)
			}
		})
	}
}

// TestReplyMaxSentences 最终回复的句数上限：缺省 5；显式 0 = 关闭排版；负数非法。
func TestReplyMaxSentences(t *testing.T) {
	const minimal = `runtime:
  llm_timeout_sec: 20
  max_msg_rune: 500
llm:
  model_name: "m"
`
	cfg, err := loadWith(t, minimal, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if got := cfg.LLMConfig.ReplyMaxSentences; got != 5 {
		t.Fatalf("缺省句数上限 = %d，期望 5", got)
	}

	cfg, err = loadWith(t, minimal+"  reply_max_sentences: 0\n", "")
	if err != nil {
		t.Fatalf("显式关闭不应报错：%v", err)
	}
	if got := cfg.LLMConfig.ReplyMaxSentences; got != 0 {
		t.Fatalf("显式 0 应关闭排版，实际 %d", got)
	}

	if _, err := loadWith(t, minimal+"  reply_max_sentences: -1\n", ""); err == nil {
		t.Fatal("负数应被拒绝")
	}
}

// TestReplyRewriteMinRunes 排版字数下限：缺省 60；显式 0 = 不设下限；负数非法。
func TestReplyRewriteMinRunes(t *testing.T) {
	const minimal = `runtime:
  llm_timeout_sec: 20
  max_msg_rune: 500
llm:
  model_name: "m"
`
	cfg, err := loadWith(t, minimal, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if got := cfg.LLMConfig.ReplyRewriteMinRunes; got != 60 {
		t.Fatalf("缺省字数下限 = %d，期望 60", got)
	}

	cfg, err = loadWith(t, minimal+"  reply_rewrite_min_runes: 0\n", "")
	if err != nil {
		t.Fatalf("显式 0 不应报错：%v", err)
	}
	if got := cfg.LLMConfig.ReplyRewriteMinRunes; got != 0 {
		t.Fatalf("显式 0 应保留，实际 %d", got)
	}

	if _, err := loadWith(t, minimal+"  reply_rewrite_min_runes: -1\n", ""); err == nil {
		t.Fatal("负数应被拒绝")
	}
}

// TestImageNoteConfig 入库图片描述参数：缺省值、显式 0 语义、负数非法。
func TestImageNoteConfig(t *testing.T) {
	const minimal = `runtime:
  llm_timeout_sec: 20
  max_msg_rune: 500
llm:
  model_name: "m"
`
	cfg, err := loadWith(t, minimal, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if got := cfg.LLMConfig.ImageNoteMaxSide; got != 640 {
		t.Fatalf("缺省长边 = %d，期望 640", got)
	}
	if got := cfg.LLMConfig.ImageNoteMaxPixels; got != 400000 {
		t.Fatalf("缺省像素上限 = %d，期望 400000", got)
	}
	if got := cfg.LLMConfig.ImageNoteTimeoutSec; got != 15 {
		t.Fatalf("缺省超时 = %d，期望 15", got)
	}
	if got := cfg.LLMConfig.ImageNoteRetries; got != 2 {
		t.Fatalf("缺省重试 = %d，期望 2", got)
	}
	if got := cfg.LLMConfig.ImageNoteConcurrency; got != 3 {
		t.Fatalf("缺省并发 = %d，期望 3", got)
	}

	cfg, err = loadWith(t, minimal+`  image_note_max_side: 0
  image_note_max_pixels: 0
  image_note_timeout_sec: 0
  image_note_retries: 0
  image_note_concurrency: 0
`, "")
	if err != nil {
		t.Fatalf("显式 0 不应报错：%v", err)
	}
	if cfg.LLMConfig.ImageNoteMaxSide != 0 || cfg.LLMConfig.ImageNoteMaxPixels != 0 ||
		cfg.LLMConfig.ImageNoteTimeoutSec != 0 || cfg.LLMConfig.ImageNoteRetries != 0 ||
		cfg.LLMConfig.ImageNoteConcurrency != 0 {
		t.Fatalf("显式 0 应全部保留，实际 %+v", cfg.LLMConfig)
	}

	for _, key := range []string{
		"image_note_max_side", "image_note_max_pixels", "image_note_timeout_sec",
		"image_note_retries", "image_note_concurrency",
	} {
		if _, err := loadWith(t, minimal+"  "+key+": -1\n", ""); err == nil {
			t.Fatalf("%s 负数应被拒绝", key)
		}
	}
}

// TestReplySendInterval 分句发送间隔：缺省 800ms；显式 0 = 不拆分；负数非法。
func TestReplySendInterval(t *testing.T) {
	const minimal = `runtime:
  llm_timeout_sec: 20
  max_msg_rune: 500
llm:
  model_name: "m"
`
	cfg, err := loadWith(t, minimal, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if got := cfg.ReplySendInterval; got != 800*time.Millisecond {
		t.Fatalf("缺省间隔 = %v，期望 800ms", got)
	}

	zero := strings.Replace(minimal, "  max_msg_rune: 500\n", "  max_msg_rune: 500\n  reply_send_interval_ms: 0\n", 1)
	cfg, err = loadWith(t, zero, "")
	if err != nil {
		t.Fatalf("显式 0 不应报错：%v", err)
	}
	if got := cfg.ReplySendInterval; got != 0 {
		t.Fatalf("显式 0 应关闭拆分，实际 %v", got)
	}

	neg := strings.Replace(minimal, "  max_msg_rune: 500\n", "  max_msg_rune: 500\n  reply_send_interval_ms: -1\n", 1)
	if _, err := loadWith(t, neg, ""); err == nil {
		t.Fatal("负数应被拒绝")
	}
}
