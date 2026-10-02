package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

// newCORSTestEngine 挂上 CORSMiddleware 并注册一个最小路由。
func newCORSTestEngine(allowedOrigins []string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(CORSMiddleware(allowedOrigins))
	engine.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
	return engine
}

func doCORSRequest(t *testing.T, engine *gin.Engine, method, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, "/ping", nil)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func TestCORS默认不下发跨域头(t *testing.T) {
	engine := newCORSTestEngine(nil)

	rec := doCORSRequest(t, engine, http.MethodGet, "http://evil.example.com")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("未配置来源时不应下发 ACAO，实际 %q（回到了放行任意来源的旧行为）", got)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("同源以外的简单请求仍应正常响应，实际状态码 %d", rec.Code)
	}
}

func TestCORS只放行白名单内的来源(t *testing.T) {
	engine := newCORSTestEngine([]string{"http://localhost:5173", "https://panel.example.com"})

	allowed := doCORSRequest(t, engine, http.MethodGet, "http://localhost:5173")
	if got := allowed.Header().Get("Access-Control-Allow-Origin"); got != "http://localhost:5173" {
		t.Fatalf("白名单内的来源应被反射，实际 %q", got)
	}
	if got := allowed.Header().Get("Vary"); got != "Origin" {
		t.Fatalf("反射来源时必须带 Vary: Origin，实际 %q", got)
	}

	denied := doCORSRequest(t, engine, http.MethodGet, "http://evil.example.com")
	if got := denied.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("白名单外的来源不应被放行，实际 %q", got)
	}
}

func TestCORS通配来源(t *testing.T) {
	engine := newCORSTestEngine([]string{"*"})

	rec := doCORSRequest(t, engine, http.MethodGet, "http://any.example.com")
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("配了 * 应放行任意来源，实际 %q", got)
	}
}

func TestCORS预检直接结束(t *testing.T) {
	engine := newCORSTestEngine([]string{"http://localhost:5173"})

	rec := doCORSRequest(t, engine, http.MethodOptions, "http://localhost:5173")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("预检应返回 204，实际 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("预检不应进业务路由，响应体应为空，实际 %q", rec.Body.String())
	}
}

// ===== 探针 =====

func TestHealthz恒返回200(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{}
	engine := gin.New()
	engine.GET("/healthz", server.handleHealthz())

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("存活探针应恒为 200，实际 %d", rec.Code)
	}
}

func TestReadyz在优雅关闭时转503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &Server{}
	engine := gin.New()
	engine.GET("/readyz", server.handleReadyz())

	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("正常运行时就绪探针应为 200，实际 %d", rec.Code)
	}

	server.draining.Store(true)
	rec = httptest.NewRecorder()
	engine.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("优雅关闭中应返回 503，实际 %d", rec.Code)
	}
}
