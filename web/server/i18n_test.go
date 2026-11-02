package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"good-review-master/config"
	"good-review-master/logutil"
	"good-review-master/onebot"
	"good-review-master/telemetry"

	"github.com/gin-gonic/gin"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestMain 初始化 logger 并切到临时目录。
//
// 两个原因：语言包加载、错误处理都会写日志，未初始化会 nil panic；
// 切目录则是为了以后万一有东西按 cwd 建文件，不要落在仓库里。
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "webserver-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(dir); err != nil {
		panic(err)
	}
	logutil.SetupLogger()
	code := m.Run()
	logutil.Close()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// errorBody 是错误响应在测试里的视图，字段和 APIResponse 的 JSON 对得上。
type errorBody struct {
	Code  int    `json:"code"`
	Err   string `json:"err"`
	Msg   string `json:"msg"`
	ReqID string `json:"req_id"`
	Data  string `json:"data"`
}

// newTestLocales 在临时目录里铺一份语言文件（NewLocales 会把内嵌的出厂版本写出去）。
func newTestLocales(t *testing.T) *Locales {
	t.Helper()
	locales := NewLocales(t.TempDir())
	if locales.state.Load() == nil {
		t.Fatal("语言包没加载起来：检查 web/server/locales 下是否还有 yaml")
	}
	return locales
}

// newErrorTestEngine 复刻生产上的中间件顺序：Recovery 在外、ErrorHandler 在内。
func newErrorTestEngine(t *testing.T, locales *Locales) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(RecoveryMiddleware(locales))
	engine.Use(ErrorHandler(locales))
	engine.GET("/biz", func(c *gin.Context) { fail(c, CodeWorkspaceNotFound, nil) })
	engine.GET("/badjson", func(c *gin.Context) { fail(c, CodeBadJSON, nil) })
	engine.GET("/multiple", func(c *gin.Context) {
		fail(c, CodeMultipleWorkspaces, map[string]any{"Endpoint": "/api/workspaces/{wid}/groups"})
	})
	engine.GET("/boom", func(c *gin.Context) {
		_ = c.Error(errors.New("数据库连接被重置"))
	})
	engine.GET("/panic", func(c *gin.Context) { panic("空指针") })
	engine.GET("/written", func(c *gin.Context) {
		c.JSON(http.StatusOK, APIResponse{Code: 200, Data: "done"})
		_ = c.Error(errors.New("收尾时的次要错误"))
	})
	return engine
}

func doGet(t *testing.T, engine *gin.Engine, path string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for key, value := range header {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

func doPostJSON(t *testing.T, engine *gin.Engine, path, payload string, header map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for key, value := range header {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	engine.ServeHTTP(rec, req)
	return rec
}

// newLoginTestEngine 挂上官方的登录中间件链，用来验证登录失败的那两条路。
func newLoginTestEngine(locales *Locales, limiter *loginRateLimiter) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(RecoveryMiddleware(locales))
	engine.Use(ErrorHandler(locales))
	engine.POST("/login", handleLogin("admin", "secret", "test-sign-key", limiter))
	return engine
}

func decodeErrorBody(t *testing.T, rec *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是合法 JSON（%v）：%s", err, rec.Body.String())
	}
	return body
}

// Test每个错误码都有中英文案 是这张码表唯一的守门人：
// 加了码却忘了写文案，线上会悄悄回落到"未知错误"，光看代码发现不了。
func Test每个错误码都有中英文案(t *testing.T) {
	locales := newTestLocales(t)
	state := locales.state.Load()
	// 模板参数给一份全的：带 {{.Endpoint}} 的条目没有参数也能渲染，
	// 但这里只关心"条目存不存在"，拿 error 判断。
	args := map[string]any{"Endpoint": "/api/x"}

	for _, code := range allErrCodes() {
		id := code.messageID()
		for _, lang := range []string{"zh-CN", "en-US"} {
			msg, err := localizeRaw(state.bundle, lang, id, args)
			if err != nil {
				t.Errorf("码 %d（%s）在 %s 里查不到：%v", int(code), id, lang, err)
				continue
			}
			if strings.TrimSpace(msg) == "" {
				t.Errorf("码 %d（%s）在 %s 里的文案是空串", int(code), id, lang)
			}
		}
	}
}

// localizeRaw 与 localizeOne 查的是同一件事，区别是把它底下的 error 抛出来——
// 测试要的是"缺了没有"，而不是"缺了就静默兜底"。
func localizeRaw(bundle *i18n.Bundle, tag, id string, args map[string]any) (string, error) {
	return i18n.NewLocalizer(bundle, tag).Localize(&i18n.LocalizeConfig{
		MessageID:    id,
		TemplateData: args,
	})
}

func Test业务错误走200并带上翻译好的文案(t *testing.T) {
	engine := newErrorTestEngine(t, newTestLocales(t))

	rec := doGet(t, engine, "/biz", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("业务判定失败应该是 HTTP 200（HTTP 层没出错），实际 %d", rec.Code)
	}
	body := decodeErrorBody(t, rec)
	if body.Code != int(CodeWorkspaceNotFound) {
		t.Fatalf("code 应为 %d，实际 %d", int(CodeWorkspaceNotFound), body.Code)
	}
	if body.Err != "workspace.not_found" {
		t.Fatalf("err 应为 workspace.not_found，实际 %q", body.Err)
	}
	if body.Msg != "工作空间不存在" {
		t.Fatalf("msg 应由后端翻好，实际 %q", body.Msg)
	}

	en := decodeErrorBody(t, doGet(t, engine, "/biz", map[string]string{LangHeader: "en-US"}))
	if en.Msg != "Workspace not found" {
		t.Fatalf("带 X-Lang: en-US 时应返回英文，实际 %q", en.Msg)
	}
	if en.Code != body.Code {
		t.Fatalf("换了语言不该换错误码，%d != %d", en.Code, body.Code)
	}
}

// Test业务失败会被指标记下来 守的是看板最后那块曲线：
//
// 中间件漏报、或者 route 写成真实路径、code 写成另一个值，面板上就是一条永远平的线，
// 而且不会有任何报错——这种“指标名对得上但永远没数据”的坑只能测出来。
func Test业务失败会被指标记下来(t *testing.T) {
	locales := newTestLocales(t)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(RecoveryMiddleware(locales))
	engine.Use(TelemetryMiddleware())
	engine.Use(ErrorHandler(locales))
	// 用一条别的用例不碰的路由，标签组合是干净的（全局计数器不做隔离）
	engine.GET("/__metric_probe", func(c *gin.Context) { fail(c, CodeWorkspaceNotFound, nil) })

	route, code := "/__metric_probe", strconv.Itoa(int(CodeWorkspaceNotFound))
	counter := telemetry.HTTPBusinessErrorsTotal.WithLabelValues(route, code)
	before := testutil.ToFloat64(counter)

	doGet(t, engine, "/__metric_probe", nil)

	// 看增量而不是绝对值：同一个标签组合可能被 -count 重复跑到
	if delta := testutil.ToFloat64(counter) - before; delta != 1 {
		t.Fatalf("业务失败应让 http_business_errors_total{route=%s,code=%s} 加 1，实际加 %v", route, code, delta)
	}
}

func Test带参数的文案会把参数填进去(t *testing.T) {
	engine := newErrorTestEngine(t, newTestLocales(t))

	body := decodeErrorBody(t, doGet(t, engine, "/multiple", nil))
	if !strings.Contains(body.Msg, "/api/workspaces/{wid}/groups") {
		t.Fatalf("文案里的 Endpoint 没有被替换，实际 %q", body.Msg)
	}
	if strings.Contains(body.Msg, "{{") {
		t.Fatalf("模板语法漏到了客户端，实际 %q", body.Msg)
	}
}

func Test请求格式错误仍然是HTTP400(t *testing.T) {
	engine := newErrorTestEngine(t, newTestLocales(t))

	rec := doGet(t, engine, "/badjson", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("请求本身不合法应发 HTTP 400（没走完业务代码），实际 %d", rec.Code)
	}
	// 但 body 里照样有 code/err/msg：前端两条路都看得到同一种结构
	body := decodeErrorBody(t, rec)
	if body.Code != int(CodeBadJSON) || body.Msg == "" {
		t.Fatalf("400 的响应体也应该带 code 和 msg，实际 %+v", body)
	}
}

func Test未预期的错误压成一段兜底文案(t *testing.T) {
	engine := newErrorTestEngine(t, newTestLocales(t))

	rec := doGet(t, engine, "/boom", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("未预期错误应该是 HTTP 500，实际 %d", rec.Code)
	}
	body := decodeErrorBody(t, rec)
	if body.Code != int(CodeInternal) {
		t.Fatalf("code 应为兜底码 %d，实际 %d", int(CodeInternal), body.Code)
	}
	if strings.Contains(body.Msg, "数据库") || strings.Contains(rec.Body.String(), "数据库") {
		t.Fatalf("底层错误原文不该出现在响应里（会泄露内部细节）：%s", rec.Body.String())
	}
}

func TestPanic也返回结构化响应(t *testing.T) {
	engine := newErrorTestEngine(t, newTestLocales(t))

	rec := doGet(t, engine, "/panic", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应该是 HTTP 500，实际 %d", rec.Code)
	}
	// 关键：body 是合法 JSON，前端能正常解析并弹窗，而不是拿到一行 "Internal Server Error"
	body := decodeErrorBody(t, rec)
	if body.Code != int(CodeInternal) || body.Msg == "" {
		t.Fatalf("panic 的响应应该是完整信封，实际 %+v", body)
	}
}

func Test响应写出去之后不再追加错误(t *testing.T) {
	engine := newErrorTestEngine(t, newTestLocales(t))

	rec := doGet(t, engine, "/written", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("已经正常返回的请求不该被改写成错误码，实际 %d", rec.Code)
	}
	// 再写一遍会把 body 拼成两段 JSON，前端解析必炸
	body := decodeErrorBody(t, rec)
	if body.Data != "done" {
		t.Fatalf("原有响应体应保持原样，实际 %+v", body)
	}
}

func Test语言协商优先级(t *testing.T) {
	locales := newTestLocales(t)

	cases := []struct {
		name   string
		header map[string]string
		want   string
	}{
		{"X-Lang 压过 Accept-Language", map[string]string{
			LangHeader: "en-US", "Accept-Language": "zh-CN",
		}, "en-US"},
		{"没有 X-Lang 时用浏览器语言", map[string]string{
			"Accept-Language": "en-US,en;q=0.9",
		}, "en-US"},
		{"主语言能对上地区语言", map[string]string{LangHeader: "en"}, "en-US"},
		{"都不认识时落到默认语言", map[string]string{
			LangHeader: "ja-JP", "Accept-Language": "ko-KR",
		}, "zh-CN"},
		{"什么都没带也是默认语言", nil, "zh-CN"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for key, value := range tc.header {
				req.Header.Set(key, value)
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = req
			if got := locales.requestLang(c); got != tc.want {
				t.Fatalf("语言应为 %s，实际 %s", tc.want, got)
			}
		})
	}
}

func Test登录密码错是业务码不是HTTP401(t *testing.T) {
	locales := newTestLocales(t)
	engine := newLoginTestEngine(locales, newLoginRateLimiter())

	payload := `{"username":"admin","password":"wrong"}`
	rec := doPostJSON(t, engine, "/login", payload, nil)
	if rec.Code != http.StatusOK {
		// 401 是留给“登录态失效”的，密码错是业务判断，混用会让前端把
		// 刚输错一次密码的人直接踢回登录页
		t.Fatalf("密码错应该是 HTTP 200，实际 %d", rec.Code)
	}
	body := decodeErrorBody(t, rec)
	if body.Code != int(CodeBadCredentials) {
		t.Fatalf("code 应为 %d，实际 %d", int(CodeBadCredentials), body.Code)
	}
	if body.Msg != "账号或密码错误" {
		t.Fatalf("默认应返回中文文案，实际 %q", body.Msg)
	}

	en := decodeErrorBody(t, doPostJSON(t, engine, "/login", payload, map[string]string{LangHeader: "en-US"}))
	if en.Msg != "Incorrect username or password" {
		t.Fatalf("带 X-Lang: en-US 时应返回英文，实际 %q", en.Msg)
	}
}

func Test登录被限流时保持429和RetryAfter(t *testing.T) {
	locales := newTestLocales(t)
	// 桶容量 3、不回填：前 3 次只记录失败，第 4 次起被拦
	engine := newLoginTestEngine(locales, newLoginRateLimiterWith(0, 3))

	payload := `{"username":"admin","password":"wrong"}`
	var rec *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		rec = doPostJSON(t, engine, "/login", payload, nil)
		if rec.Code == http.StatusTooManyRequests {
			break
		}
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("反复输错后应被限流（HTTP 429），实际 %d", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "60" {
		t.Fatalf("限流必须带 Retry-After（网关和浏览器都认这个头），实际 %q", got)
	}
	body := decodeErrorBody(t, rec)
	if body.Code != int(CodeRateLimited) || body.Msg == "" {
		t.Fatalf("429 的响应体也该带业务码和文案，实际 %+v", body)
	}
}

// Test真实服务上登录失败走信封 走的是 server.New 装配出来的那条真链路。
//
// 前面那些用例都是自己搭的 engine，只能证明中间件本身对；证明不了
// "ErrorHandler 挂没挂在 /api 组上""locales 有没有从 work path 加载起来"
// ——这两处漏了，线上就是 404 或者一句英文的 Internal Server Error。
func Test真实服务上登录失败走信封(t *testing.T) {
	isolateWorkdir(t)
	logutil.SetupLogger()

	port := freeTCPPort(t)
	srv := New(staticSnapshot(&config.Config{
		WebPort:     port,
		WebUsername: "admin",
		WebPassword: "pw",
		JWTSecret:   "test-secret",
	}), onebot.NewClient("", nil, ""), nil)

	go func() { _ = srv.Start() }()
	waitForServer(t, port)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})

	post := func(lang string) (int, errorBody) {
		t.Helper()
		req, err := http.NewRequest(
			http.MethodPost,
			baseURL(port)+"/api/login",
			strings.NewReader(`{"username":"admin","password":"wrong"}`),
		)
		if err != nil {
			t.Fatalf("构造请求失败：%v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if lang != "" {
			req.Header.Set(LangHeader, lang)
		}

		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("请求登录接口失败：%v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("读响应体失败：%v", err)
		}

		var body errorBody
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("响应不是信封 JSON（%v）：%s", err, raw)
		}
		return resp.StatusCode, body
	}

	status, zh := post("")
	if status != http.StatusOK {
		t.Fatalf("密码错应是 HTTP 200，实际 %d", status)
	}
	if zh.Code != int(CodeBadCredentials) {
		t.Fatalf("code 应为 %d，实际 %d", int(CodeBadCredentials), zh.Code)
	}
	if zh.Msg != "账号或密码错误" {
		t.Fatalf("默认语言的中文文案没出来，实际 %q（语言包是不是没从 work path 加载到）", zh.Msg)
	}

	_, en := post("en-US")
	if en.Msg != "Incorrect username or password" {
		t.Fatalf("带 X-Lang: en-US 应返回英文，实际 %q", en.Msg)
	}
}

func Test改语言文件不用重启(t *testing.T) {
	dir := t.TempDir()
	locales := NewLocales(dir)

	before := locales.text("en-US", "auth.bad_credentials", nil)
	if before == "" {
		t.Fatal("出厂文案没加载到，后面的断言没有意义")
	}

	path := filepath.Join(dir, "en-US.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读语言文件失败：%v", err)
	}
	updated := strings.Replace(string(raw), before, "Wrong user name or password", 1)
	if updated == string(raw) {
		t.Fatalf("没在文件里找到 %q，用例本身要跟着文案改", before)
	}
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatalf("写语言文件失败：%v", err)
	}

	locales.Reload()
	if got := locales.text("en-US", "auth.bad_credentials", nil); got != "Wrong user name or password" {
		t.Fatalf("重载后应拿到新文案，实际 %q", got)
	}
}

func Test新增一门语言不用改代码(t *testing.T) {
	dir := t.TempDir()
	locales := NewLocales(dir)

	// 只丢了下面这一条，其余条目都缺——缺的应该回落到默认语言，而不是空串
	japanese := "auth.bad_credentials: \"ユーザー名またはパスワードが違います\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ja-JP.yaml"), []byte(japanese), 0o644); err != nil {
		t.Fatalf("写语言文件失败：%v", err)
	}
	locales.Reload()

	if !locales.has("ja-JP") {
		t.Fatal("目录里多了一份 yaml 就应该被认成一门语言，实际没加载到")
	}
	if got := locales.text("ja-JP", "auth.bad_credentials", nil); !strings.Contains(got, "パスワード") {
		t.Fatalf("日语条目没生效，实际 %q", got)
	}
	if got := locales.text("ja-JP", "workspace.not_found", nil); got != "工作空间不存在" {
		t.Fatalf("缺条目应回落到默认语言，实际 %q", got)
	}
}

func Test语言文件全写坏时继续用旧版(t *testing.T) {
	dir := t.TempDir()
	locales := NewLocales(dir)

	before := locales.text("zh-CN", "auth.bad_credentials", nil)
	// 未闭合的 flow seq，yaml 解析必失败
	broken := "auth.bad_credentials: [未闭合\n"
	for _, name := range []string{"zh-CN.yaml", "en-US.yaml"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(broken), 0o644); err != nil {
			t.Fatalf("写坏文件失败：%v", err)
		}
	}

	locales.Reload()
	if got := locales.text("zh-CN", "auth.bad_credentials", nil); got != before {
		t.Fatalf("全部文件都坏掉时应保留上一版，实际 %q（是不是把旧数据清空了）", got)
	}
}
