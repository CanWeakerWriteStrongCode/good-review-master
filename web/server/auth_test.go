package server

import (
	"encoding/base64"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestParseJWT接受自己签发的token(t *testing.T) {
	token, err := GenerateJWT("admin", "topsecret")
	if err != nil {
		t.Fatalf("签发失败：%v", err)
	}
	claims, err := ParseJWT(token, "topsecret")
	if err != nil {
		t.Fatalf("校验自己签发的 token 失败：%v", err)
	}
	if sub, _ := claims["sub"].(string); sub != "admin" {
		t.Fatalf("sub 应为 admin，实际 %q", sub)
	}
}

func TestParseJWT密钥不符时拒绝(t *testing.T) {
	token, err := GenerateJWT("admin", "topsecret")
	if err != nil {
		t.Fatalf("签发失败：%v", err)
	}
	if _, err := ParseJWT(token, "别的密钥"); err == nil {
		t.Fatal("换一个密钥仍然通过校验，签名验证形同虚设")
	}
}

// TestParseJWT拒绝algNone 把"alg=none 永远不能通过"钉成不变量。
//
// 注意这条**不是**在验证 WithValidMethods 的效果：实测把它去掉这测试照样过，
// 因为 jwt/v5 的 keyfunc 返回 []byte，none 与 RS256 的验签都会因密钥类型不符而失败。
// 它的价值是回归护栏——将来升级 jwt 库或改写 keyfunc 时，
// 一旦 alg=none 变得可以通过，这里会立刻红。
func TestParseJWT拒绝algNone(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"admin"}`))
	noneToken := header + "." + claims + "."

	if _, err := ParseJWT(noneToken, "topsecret"); err == nil {
		t.Fatal("alg=none 的 token 通过了校验，存在算法混淆漏洞")
	}
}

func TestLoginRateLimiter成功登录不消费令牌(t *testing.T) {
	limiter := newLoginRateLimiter()
	const ip = "10.0.0.1"

	// 只查不记账（等价于反复登录成功），查多少次都不该被限流
	for i := 0; i < 200; i++ {
		if limiter.Blocked(ip) {
			t.Fatalf("第 %d 次查询就被限流，成功登录被误伤", i+1)
		}
	}
}

func TestLoginRateLimiter连续失败后被限流(t *testing.T) {
	limiter := newLoginRateLimiter()
	const ip = "10.0.0.2"

	for i := 0; i < loginFailBurst; i++ {
		if limiter.Blocked(ip) {
			t.Fatalf("第 %d 次失败前就被限流，桶容量与 loginFailBurst 不一致", i+1)
		}
		limiter.RecordFailure(ip)
	}
	if !limiter.Blocked(ip) {
		t.Fatalf("连续失败 %d 次后仍未被限流", loginFailBurst)
	}
}

// TestLoginRateLimiter限流会恢复 防止把限流做成永久封禁。
func TestLoginRateLimiter限流会恢复(t *testing.T) {
	// 回填 100 个/秒：桶空后约 20ms 就能攒回一个令牌
	limiter := newLoginRateLimiterWith(rate.Limit(100), 2)
	const ip = "10.0.0.3"

	limiter.RecordFailure(ip)
	limiter.RecordFailure(ip)
	if !limiter.Blocked(ip) {
		t.Fatal("桶应为空")
	}

	time.Sleep(50 * time.Millisecond)
	if limiter.Blocked(ip) {
		t.Fatal("令牌已回填却仍被限流，限流变成了永久封禁")
	}
}

func TestLoginRateLimiter不同IP互不影响(t *testing.T) {
	limiter := newLoginRateLimiter()
	for i := 0; i < loginFailBurst; i++ {
		limiter.RecordFailure("10.0.0.4")
	}
	if !limiter.Blocked("10.0.0.4") {
		t.Fatal("被爆破的 IP 应被限流")
	}
	if limiter.Blocked("10.0.0.5") {
		t.Fatal("限流串到了别的 IP，桶没有按 IP 隔离")
	}
}

// TestLoginRateLimiter回收闲置IP 保证 visitors 不会随 IP 数无限增长。
func TestLoginRateLimiter回收闲置IP(t *testing.T) {
	limiter := newLoginRateLimiter()
	limiter.RecordFailure("10.0.0.6")

	// 把这个 IP 的最后活跃时间往前推，越过 TTL，并把 lastSweep 也推早以允许扫描
	limiter.visitors["10.0.0.6"].lastSeen = time.Now().Add(-2 * loginVisitorTTL)
	limiter.lastSweep = time.Now().Add(-2 * loginSweepEvery)

	limiter.RecordFailure("10.0.0.7") // 触发一次扫描

	if _, ok := limiter.visitors["10.0.0.6"]; ok {
		t.Fatal("闲置超过 TTL 的 IP 未被回收，visitors 会无限增长")
	}
	if _, ok := limiter.visitors["10.0.0.7"]; !ok {
		t.Fatal("活跃 IP 被误回收")
	}
}
