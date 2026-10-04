package server

import (
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/time/rate"
)

const jwtExpire = 24 * time.Hour

// jwtSigningMethod JWT 签名算法。签发与校验都锁死它。
//
// 关于"锁不锁"的实际风险（实测过，别照抄网上的说法）：
// jwt/v5 默认**已经**挡住了 alg=none（keyfunc 返回 []byte，none 验签会失败），
// 也挡住了 RS256→HMAC 的算法混淆（RS256 验签要求 *rsa.PublicKey，拿到 []byte 直接报错）。
// 所以这里不是"修了一个正在被利用的漏洞"，而是纵深防御：
// 把"只认 HS256"变成显式契约，万一将来 keyfunc 改成按 alg 分支的写法，这层还在。
const jwtSigningMethod = "HS256"

// GenerateJWT 签发 JWT token
func GenerateJWT(username, secret string) (string, error) {
	claims := jwt.MapClaims{
		"sub": username,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(jwtExpire).Unix(),
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

// ParseJWT 校验 JWT token，成功返回 claims。
// 显式限定只接受 HS256——见 jwtSigningMethod 上方关于"这层到底挡了什么"的说明。
func ParseJWT(tokenStr, secret string) (jwt.MapClaims, error) {
	token, err := jwt.Parse(tokenStr, func(t *jwt.Token) (interface{}, error) {
		return []byte(secret), nil
	}, jwt.WithValidMethods([]string{jwtSigningMethod}))
	if err != nil {
		return nil, err
	}
	if claims, ok := token.Claims.(jwt.MapClaims); ok && token.Valid {
		return claims, nil
	}
	return nil, jwt.ErrSignatureInvalid
}

// ===== 登录失败限流 =====

const (
	loginFailBurst  = 5                      // 允许的连续失败次数（桶容量）
	loginFailRefill = rate.Limit(5.0 / 60.0) // 恢复速率：5 次/分钟
	loginVisitorTTL = 10 * time.Minute       // 该 IP 超过这么久没再出现，就回收它的桶
	loginSweepEvery = time.Minute            // 回收扫描的最小间隔
)

// loginRateLimiter 按来源 IP 分桶的登录限流器。
//
// 语义上只统计**失败**：成功登录不消费令牌（Blocked 只读，失败才 RecordFailure），
// 所以正常用户永远不会被自己限流，而暴力破解在若干次失败后必须等桶回填。
//
// 桶按 IP 存放，需要在请求量之外有界：sweepLocked 会惰性回收长期不活跃的 IP，
// 无需额外 goroutine（本结构体没有生命周期，跟着 Server 一起被回收）。
type loginRateLimiter struct {
	mu        sync.Mutex
	visitors  map[string]*loginVisitor
	lastSweep time.Time
	refill    rate.Limit // 令牌回填速率
	burst     int        // 单个 IP 的桶容量（=允许的连续失败次数）
}

type loginVisitor struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

func newLoginRateLimiter() *loginRateLimiter {
	return newLoginRateLimiterWith(loginFailRefill, loginFailBurst)
}

// newLoginRateLimiterWith 允许注入回填速率与桶容量。
// 存在的唯一理由是让"桶会随时间回填"可测——用默认的 5 次/分钟，
// 测一次恢复要等十几秒，那样的测试没人会留着。
func newLoginRateLimiterWith(refill rate.Limit, burst int) *loginRateLimiter {
	return &loginRateLimiter{
		refill:    refill,
		burst:     burst,
		visitors:  make(map[string]*loginVisitor),
		lastSweep: time.Now(),
	}
}

// Blocked 报告该 IP 现在是否已用完失败额度。不消费令牌。
func (l *loginRateLimiter) Blocked(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked()
	// 不建桶：只读查询不该为陌生 IP 分配内存
	v, ok := l.visitors[ip]
	if !ok {
		return false
	}
	v.lastSeen = time.Now()
	return v.limiter.TokensAt(time.Now()) < 1
}

// RecordFailure 记一次登录失败，消费一个令牌。
func (l *loginRateLimiter) RecordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked()
	v := l.visitorLocked(ip)
	// 桶空时 Allow 返回 false，但这里不关心返回值——拒绝已经由 Blocked 决定了
	_ = v.limiter.Allow()
}

func (l *loginRateLimiter) visitorLocked(ip string) *loginVisitor {
	v, ok := l.visitors[ip]
	if !ok {
		v = &loginVisitor{limiter: rate.NewLimiter(l.refill, l.burst)}
		l.visitors[ip] = v
	}
	v.lastSeen = time.Now()
	return v
}

// sweepLocked 惰性回收长时间不活跃的 IP。调用方必须持有 l.mu。
func (l *loginRateLimiter) sweepLocked() {
	now := time.Now()
	if now.Sub(l.lastSweep) < loginSweepEvery {
		return
	}
	l.lastSweep = now
	for ip, v := range l.visitors {
		if now.Sub(v.lastSeen) > loginVisitorTTL {
			delete(l.visitors, ip)
		}
	}
}
