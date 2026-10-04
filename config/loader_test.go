package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"good-review-master/logutil"
)

// TestMain 把 cwd 切到临时目录：logutil.SetupLogger 会在 apppath.ExeDir()（优先 cwd）
// 下建 log/，而 go test 的 cwd 就是包目录——不隔离就会在源码树里留下 config/log/。
// 同时必须真的初始化 logger：本包的若干路径（密钥迁移提示等）会打 WARN，
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

// baseConfig 一份能通过全部校验的最小配置。
const baseConfig = `
napcat:
  http_api: "http://127.0.0.1:3000"
bot:
  qq: "123456"
  allow_groups: "10001,10002"
runtime:
  llm_send_count: 3
  llm_timeout_sec: 20
  max_msg_rune: 500
  poll_interval_sec: 3
  web_port: 9090
  web_username: "admin"
  web_password: "123456"
llm:
  provider: "openai"
  api_key: "sk-legacy"
  api_base: "https://api.example.com"
  model_name: "test-model"
  temperature: 1.0
  top_p: 0.95
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
	sources := Sources{Config: writeFile(t, "config.yaml", configYAML)}
	if secretYAML != "" {
		sources.Secret = writeFile(t, "secret.yaml", secretYAML)
	}
	return Load(sources)
}

func TestLoad基本配置(t *testing.T) {
	cfg, err := loadWith(t, baseConfig, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.BotQQ != "123456" {
		t.Errorf("BotQQ = %q", cfg.BotQQ)
	}
	if len(cfg.AllowGroups) != 2 || cfg.AllowGroups[0] != "10001" {
		t.Errorf("AllowGroups = %v", cfg.AllowGroups)
	}
	if cfg.PollInterval.Seconds() != 3 {
		t.Errorf("PollInterval = %v", cfg.PollInterval)
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
  qq: "1"
  allow_groups: "1"
runtime:
  llm_timeout_sec: 20
  max_msg_rune: 500
  poll_interval_sec: 3
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

func TestSecret优先于config旧位置(t *testing.T) {
	cfg, err := loadWith(t, baseConfig, `
llm_api_key: "sk-from-secret-file"
web_password: "pw-from-secret-file"
napcat_access_token: "nap-from-secret-file"
`)
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.LLMConfig.APIKey != "sk-from-secret-file" {
		t.Errorf("secret.yaml 应优先，实际 APIKey = %q", cfg.LLMConfig.APIKey)
	}
	if cfg.WebPassword != "pw-from-secret-file" {
		t.Errorf("secret.yaml 应优先，实际 WebPassword = %q", cfg.WebPassword)
	}
	if cfg.NapCatAccessToken != "nap-from-secret-file" {
		t.Errorf("secret.yaml 应优先，实际 NapCatAccessToken = %q", cfg.NapCatAccessToken)
	}
}

// TestSecret缺失时回退旧位置 保证存量配置（密钥还写在 config.yaml 里）继续能跑。
func TestSecret缺失时回退旧位置(t *testing.T) {
	cfg, err := loadWith(t, baseConfig, "")
	if err != nil {
		t.Fatalf("没有 secret.yaml 也应能加载：%v", err)
	}
	if cfg.LLMConfig.APIKey != "sk-legacy" {
		t.Errorf("应回退到 config.yaml 的旧位置，实际 APIKey = %q", cfg.LLMConfig.APIKey)
	}
	if cfg.WebPassword != "123456" {
		t.Errorf("应回退到 config.yaml 的旧位置，实际 WebPassword = %q", cfg.WebPassword)
	}
}

func Test环境变量优先于两者(t *testing.T) {
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

	cfg, err = loadWith(t, baseConfig, "jwt_secret: \"dedicated\"\n")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.JWTSecret != "dedicated" {
		t.Errorf("配了 jwt_secret 就不该回退，实际 %q", cfg.JWTSecret)
	}
}

func TestMCPToken优先级(t *testing.T) {
	configYAML := baseConfig + `
mcp:
  enabled: true
  servers:
    - name: "svc"
      transport: "http"
      url: "https://example.com/mcp"
      token: "token-in-config"
`
	cfg, err := loadWith(t, configYAML, "mcp_server_tokens:\n  svc: \"token-in-secret\"\n")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if len(cfg.MCPConfig.Servers) != 1 {
		t.Fatalf("应有 1 个服务，实际 %d", len(cfg.MCPConfig.Servers))
	}
	if got := cfg.MCPConfig.Servers[0].Token; got != "token-in-secret" {
		t.Errorf("secret.yaml 的 mcp_server_tokens 应优先，实际 %q", got)
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
		wantContain string
	}{
		{
			name: "poll_interval_sec 为 0 会 panic NewTicker",
			configYAML: strings.Replace(baseConfig,
				"  poll_interval_sec: 3", "  poll_interval_sec: 0", 1),
			wantContain: "poll_interval_sec",
		},
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
			name: "napcat 地址没有 scheme",
			configYAML: strings.Replace(baseConfig,
				`  http_api: "http://127.0.0.1:3000"`, `  http_api: "127.0.0.1:3000"`, 1),
			wantContain: "http_api",
		},
		{
			name: "开了 web_port 却没有密码",
			configYAML: strings.Replace(baseConfig,
				`  web_password: "123456"`, "", 1),
			wantContain: "web",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := loadWith(t, testCase.configYAML, "")
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
	first, err := Load(Sources{Config: path})
	if err != nil {
		t.Fatalf("首次加载失败：%v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := Load(Sources{Config: path})
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
	before := dirEntries(t, dir)

	if _, err := Load(Sources{Config: path, Secret: filepath.Join(dir, "secret.yaml")}); err != nil {
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
	if a.NapCatHTTPAPI != b.NapCatHTTPAPI || a.NapCatAccessToken != b.NapCatAccessToken ||
		a.BotQQ != b.BotQQ || a.MaxCacheMsg != b.MaxCacheMsg ||
		a.LLMSendCount != b.LLMSendCount || a.LLMTimeout != b.LLMTimeout ||
		a.MaxMsgRune != b.MaxMsgRune || a.PollInterval != b.PollInterval ||
		a.WebPort != b.WebPort || a.WebUsername != b.WebUsername ||
		a.WebPassword != b.WebPassword || a.JWTSecret != b.JWTSecret ||
		a.McpBuiltinToken != b.McpBuiltinToken || a.EnablePprof != b.EnablePprof ||
		a.ShutdownDelay != b.ShutdownDelay {
		return false
	}
	if strings.Join(a.AllowGroups, ",") != strings.Join(b.AllowGroups, ",") {
		return false
	}
	if strings.Join(a.CorsOrigins, ",") != strings.Join(b.CorsOrigins, ",") {
		return false
	}
	if a.LLMConfig != b.LLMConfig {
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
