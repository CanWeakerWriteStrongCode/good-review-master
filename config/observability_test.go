package config

import (
	"strings"
	"testing"
)

// withRuntime / withLLM 把若干行插进 baseConfig 的对应段里。
//
// **不能直接把新键拼在 baseConfig 末尾**：末尾是 llm 段，键会落进 llm 里被当成
// 未知字段忽略，于是测试"通过"了却什么都没测到。本文件第一版就是这么写的，
// 被自己的断言抓了出来——所以这两个助手函数留着，顺便备忘。
func withRuntime(extra string) string {
	return strings.Replace(baseConfig, "runtime:\n", "runtime:\n"+extra, 1)
}

func withLLM(extra string) string {
	return strings.Replace(baseConfig, "llm:\n", "llm:\n"+extra, 1)
}

// TestMetricsAddr缺省打开 覆盖这里刻意的非对称设计：
// "没写" = 用缺省地址（默认开），要关得显式写 off。
// 因为留空已经被"用缺省值"占了，所以必须有哨兵值——这正是要钉住的地方。
func TestMetricsAddr缺省打开(t *testing.T) {
	cfg, err := loadWith(t, baseConfig, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.MetricsAddr != defaultMetricsAddr {
		t.Errorf("没写 metrics_addr 时应用缺省地址 %q，实际 %q", defaultMetricsAddr, cfg.MetricsAddr)
	}

	cfg, err = loadWith(t, withRuntime("  metrics_addr: \"127.0.0.1:9200\"\n"), "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.MetricsAddr != "127.0.0.1:9200" {
		t.Errorf("显式地址没生效，实际 %q", cfg.MetricsAddr)
	}
}

// TestMetricsAddr关闭写法 覆盖几种同义写法都归一成同一个哨兵值。
// 不会归一的后果是 app 侧要判五种字符串，漏一种就是"关了但没关掉"。
func TestMetricsAddr关闭写法(t *testing.T) {
	for _, written := range []string{"off", "OFF", " none ", "disabled", "false"} {
		cfg, err := loadWith(t, withRuntime("  metrics_addr: \""+written+"\"\n"), "")
		if err != nil {
			t.Fatalf("metrics_addr=%q 不该加载失败：%v", written, err)
		}
		if cfg.MetricsAddr != MetricsAddrOff {
			t.Errorf("metrics_addr=%q 应归一成 %q，实际 %q", written, MetricsAddrOff, cfg.MetricsAddr)
		}
	}
}

// TestMetricsAddr非法值拒绝：写错了要在启动时说清楚，而不是等运行期绑不上端口。
// （绑不上也只是记一条错误、不致命，所以"启动时报错"是唯一的发现机会。）
func TestMetricsAddr非法值拒绝(t *testing.T) {
	for _, written := range []string{"9100", "127.0.0.1", "http://127.0.0.1:9100"} {
		_, err := loadWith(t, withRuntime("  metrics_addr: \""+written+"\"\n"), "")
		if err == nil {
			t.Errorf("metrics_addr=%q 应当被拒绝", written)
			continue
		}
		if !strings.Contains(err.Error(), "metrics_addr") {
			t.Errorf("报错要点名是哪个字段，实际：%v", err)
		}
	}
}

// TestOtlpEndpoint校验：空 = 关闭（合法），填了就必须是合法 http(s) 地址。
func TestOtlpEndpoint校验(t *testing.T) {
	cfg, err := loadWith(t, baseConfig, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.OTLPEndpoint != "" {
		t.Errorf("没写 otlp_endpoint 时应为空（=关闭），实际 %q", cfg.OTLPEndpoint)
	}

	if _, err := loadWith(t, withRuntime("  otlp_endpoint: \"localhost:4318\"\n"), ""); err == nil {
		t.Error("缺 scheme 的端点应当被拒绝")
	}

	cfg, err = loadWith(t, withRuntime("  otlp_endpoint: \"http://localhost:4318\"\n"), "")
	if err != nil {
		t.Fatalf("合法端点不该失败：%v", err)
	}
	if cfg.OTLPEndpoint != "http://localhost:4318" {
		t.Errorf("OTLPEndpoint = %q", cfg.OTLPEndpoint)
	}
}

// Test熔断缺省与关闭写法 覆盖两个域上同一套语义：
// 不写（0）= 用缺省；-1 = 显式关闭；其它负数 = 报错。
//
// 这套 -1 的约定沿用 mcp.retry_interval_sec 已有的写法。不统一的话，
// 用户会看到"这个字段 -1 关、那个字段 0 关"，然后每次都猜错。
func Test熔断缺省与关闭写法(t *testing.T) {
	cfg, err := loadWith(t, baseConfig, "")
	if err != nil {
		t.Fatalf("加载失败：%v", err)
	}
	if cfg.LLMConfig.BreakerFailures != defaultBreakerFailures {
		t.Errorf("llm.breaker_failures 缺省应为 %d，实际 %d",
			defaultBreakerFailures, cfg.LLMConfig.BreakerFailures)
	}
	if cfg.MCPConfig.BreakerFailures != defaultMCPBreakerFailures {
		t.Errorf("mcp.breaker_failures 缺省应为 %d，实际 %d",
			defaultMCPBreakerFailures, cfg.MCPConfig.BreakerFailures)
	}

	cfg, err = loadWith(t, withLLM("  rate_limit_per_sec: 2\n  breaker_failures: -1\n"), "")
	if err != nil {
		t.Fatalf("breaker_failures=-1 是合法的（关闭熔断），不该失败：%v", err)
	}
	if cfg.LLMConfig.BreakerFailures != -1 {
		t.Errorf("显式关闭应原样传下去，实际 %d", cfg.LLMConfig.BreakerFailures)
	}
	if cfg.LLMConfig.RateLimitPerSec != 2 {
		t.Errorf("RateLimitPerSec = %v", cfg.LLMConfig.RateLimitPerSec)
	}

	if _, err := loadWith(t, withLLM("  breaker_failures: -2\n"), ""); err == nil {
		t.Error("breaker_failures=-2 应当被拒绝（只有 -1 是关闭哨兵）")
	}
	if _, err := loadWith(t, withLLM("  rate_limit_per_sec: -1\n"), ""); err == nil {
		t.Error("rate_limit_per_sec 为负应当被拒绝（0 才是关闭）")
	}
}
