package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// TestHTTPRouteLabel 锁住基数控制这条硬规矩：拿不到路由模板时也不能回落到原始 URL。
// /api/groups/<群号> 这类路径一旦进了标签，每条不同的群号都会生成一条独立时间线。
func TestHTTPRouteLabel(t *testing.T) {
	cases := map[string]string{
		"/api/groups/:id": "/api/groups/:id",
		"/healthz":        "/healthz",
		"":                UnmatchedRoute,
	}
	for input, want := range cases {
		if got := HTTPRouteLabel(input); got != want {
			t.Errorf("HTTPRouteLabel(%q) = %q，期望 %q", input, got, want)
		}
	}
}

// TestNewRegistry可重复构造 防一个很容易犯的错：
// 如果哪天有人把手写的 NewRegistry 换成 promauto 或全局默认注册表，
// 第二次调用就会因为"collector 重复注册"而 panic。测试里反复建注册表是常态。
func TestNewRegistry可重复构造(t *testing.T) {
	first := NewRegistry()
	second := NewRegistry()
	if first == second {
		t.Fatal("两次 NewRegistry 应是互不相干的实例")
	}

	// 带标签的指标（*Vec）在"还没有任何标签组合出现过"时**不会**出现在导出里——
	// 这是 Prometheus 的既定行为，不是注册失败。所以先各记一笔再检查，
	// 顺带验证了"埋点侧写进去的东西确实能被抓到"（这两件事都得成立才有意义）。
	HTTPRequestsTotal.WithLabelValues("GET", "/healthz", "200").Inc()
	LLMRequestsTotal.WithLabelValues("test-model", ResultOK).Inc()
	LLMWindowModeTotal.WithLabelValues("extend").Inc()
	MCPToolCallsTotal.WithLabelValues("server", "tool", ResultOK).Inc()
	ConfigReloadTotal.WithLabelValues("auto").Inc()

	families, err := first.Gatherer().Gather()
	if err != nil {
		t.Fatalf("Gather 失败：%v", err)
	}
	found := map[string]bool{}
	for _, family := range families {
		found[family.GetName()] = true
	}
	// 每个域抽一个代表：漏了说明 allCollectors 的清单与该域脱节了
	for _, name := range []string{
		"http_requests_total", "llm_requests_total", "llm_window_mode_total",
		"poll_cycle_duration_seconds", "mcp_tool_calls_total", "pool_queue_len",
		"config_reload_total", "go_goroutines",
	} {
		if !found[name] {
			t.Errorf("注册表里没有 %s（检查 allCollectors 是否漏了它）", name)
		}
	}
}

// TestOTLPStatus码映射 单测这张映射表。它不是装饰：
// SDK 的 codes.Code 是 Unset=0/Error=1/Ok=2，而 OTLP 是 UNSET=0/OK=1/ERROR=2——
// 顺序不一样，直接 int 转换会把"成功"报成"错误"。写错不会报错，只会让链路图上的
// 错误率永远对不上，所以必须钉住。
func TestOTLPStatus码映射(t *testing.T) {
	cases := map[codes.Code]int{
		codes.Unset: 0,
		codes.Ok:    1,
		codes.Error: 2,
	}
	for input, want := range cases {
		if got := otlpStatusCode(input); got != want {
			t.Errorf("otlpStatusCode(%v) = %d，期望 %d", input, got, want)
		}
	}
}

// TestOTLP端点归一化 覆盖"用户按惯例只写 host:port"和"已经写全了"两种写法。
func TestOTLP端点归一化(t *testing.T) {
	res := resource.NewSchemaless()

	cases := map[string]string{
		"http://localhost:4318":            "http://localhost:4318/v1/traces",
		"http://localhost:4318/":           "http://localhost:4318/v1/traces",
		"http://localhost:4318/v1/traces":  "http://localhost:4318/v1/traces",
		"http://localhost:4318/v1/traces/": "http://localhost:4318/v1/traces",
	}
	for input, want := range cases {
		exporter, err := newOTLPHTTPExporter(input, res)
		if err != nil {
			t.Fatalf("newOTLPHTTPExporter(%q) 失败：%v", input, err)
		}
		if exporter.url != want {
			t.Errorf("端点 %q 归一化成了 %q，期望 %q", input, exporter.url, want)
		}
	}

	for _, bad := range []string{"", "  ", "localhost:4318", "ftp://x/y"} {
		if _, err := newOTLPHTTPExporter(bad, res); err == nil {
			t.Errorf("端点 %q 应当被拒绝", bad)
		}
	}
}

// TestOTLP导出器发出规范JSON 是本文件最重要的一条：
// 手写的 OTLP/HTTP 导出器没有官方库兜底，序列化写错了采集端只会静默丢弃
// （Jaeger 收到畸形 JSON 就是解析失败，界面上什么都看不到）。
// 所以这里对着一个 httptest 服务把真实请求体解回来，逐字段核对规范要求。
func TestOTLP导出器发出规范JSON(t *testing.T) {
	var (
		gotPath        string
		gotContentType string
		gotBody        []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"partialSuccess":{}}`))
	}))
	defer server.Close()

	res := resource.NewSchemaless(attribute.String("service.name", "good-review-test"))
	exporter, err := newOTLPHTTPExporter(server.URL, res)
	if err != nil {
		t.Fatalf("构造导出器失败：%v", err)
	}
	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)

	_, span := provider.Tracer("test-scope").Start(context.Background(), "测试 span",
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("字符串属性", "值"),
			attribute.Int("整数属性", 3),
		),
	)
	span.SetStatus(codes.Error, "炸了")
	span.End()

	// Shutdown 会把批处理里剩下的 span 冲刷出去，这也是它必须在退出流程最后调的原因——
	// 少了这一步，最后一批链路就随进程一起没了。
	if err := provider.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown 失败：%v", err)
	}

	if gotPath != otlpTracesPath {
		t.Errorf("上报路径是 %q，期望 %q", gotPath, otlpTracesPath)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type 是 %q，OTLP/HTTP 的 JSON 编码要求 application/json", gotContentType)
	}

	var payload otlpTraceRequest
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("请求体不是合法 JSON：%v\n%s", err, gotBody)
	}
	if len(payload.ResourceSpans) != 1 {
		t.Fatalf("resourceSpans 应有 1 项，实际 %d", len(payload.ResourceSpans))
	}
	resourceSpans := payload.ResourceSpans[0]

	if got := stringAttribute(resourceSpans.Resource.Attributes, "service.name"); got != "good-review-test" {
		t.Errorf("resource 的 service.name = %q，期望 good-review-test", got)
	}
	if len(resourceSpans.ScopeSpans) != 1 {
		t.Fatalf("scopeSpans 应有 1 项，实际 %d", len(resourceSpans.ScopeSpans))
	}
	scopeSpans := resourceSpans.ScopeSpans[0]
	if scopeSpans.Scope.Name != "test-scope" {
		t.Errorf("scope.name = %q，期望 test-scope", scopeSpans.Scope.Name)
	}
	if len(scopeSpans.Spans) != 1 {
		t.Fatalf("spans 应有 1 项，实际 %d", len(scopeSpans.Spans))
	}

	got := scopeSpans.Spans[0]
	if len(got.TraceID) != 32 {
		t.Errorf("traceId 应是 32 位十六进制，实际 %q（长度 %d）", got.TraceID, len(got.TraceID))
	}
	if len(got.SpanID) != 16 {
		t.Errorf("spanId 应是 16 位十六进制，实际 %q（长度 %d）", got.SpanID, len(got.SpanID))
	}
	if got.Name != "测试 span" {
		t.Errorf("name = %q，期望 %q", got.Name, "测试 span")
	}
	if got.Kind != 3 { // SPAN_KIND_CLIENT
		t.Errorf("kind = %d，期望 3（CLIENT）", got.Kind)
	}
	if got.Status == nil || got.Status.Code != 2 {
		t.Errorf("status 应为 ERROR(2)，实际 %+v", got.Status)
	}
	if got.Status != nil && got.Status.Message != "炸了" {
		t.Errorf("status.message = %q，期望 %q", got.Status.Message, "炸了")
	}

	// 规范要求 int64 在 JSON 里是**字符串**（JSON number 装不下 int64）。
	// 时间戳与整数属性都走这条规则，写错会被采集端整批拒绝。
	start, err := strconv.ParseInt(got.StartTimeUnixNano, 10, 64)
	if err != nil {
		t.Fatalf("startTimeUnixNano 应是十进制字符串，实际 %q：%v", got.StartTimeUnixNano, err)
	}
	end, err := strconv.ParseInt(got.EndTimeUnixNano, 10, 64)
	if err != nil {
		t.Fatalf("endTimeUnixNano 应是十进制字符串，实际 %q：%v", got.EndTimeUnixNano, err)
	}
	if start <= 0 || end < start {
		t.Errorf("时间戳不合理：start=%d end=%d", start, end)
	}
	if got := intAttribute(got.Attributes, "整数属性"); got != "3" {
		t.Errorf("整数属性的 intValue = %q，期望字符串 \"3\"", got)
	}
	if got := stringAttribute(got.Attributes, "字符串属性"); got != "值" {
		t.Errorf("字符串属性 = %q，期望 %q", got, "值")
	}
}

// TestOTLP导出器失败不静默 采集端返回非 2xx 时必须报错，
// 且错误里要带上对方的说明——否则"追踪没数据"就只能靠猜。
func TestOTLP导出器失败不静默(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"字段不认识"}`))
	}))
	defer server.Close()

	exporter, err := newOTLPHTTPExporter(server.URL, resource.NewSchemaless())
	if err != nil {
		t.Fatalf("构造导出器失败：%v", err)
	}

	// 这里直接调 ExportSpans 而不是走 BatchSpanProcessor：
	// 批处理器会把导出错误吞掉（只记日志、不重试），那是它该有的行为——
	// 追踪失败绝不该影响业务。但"吞掉"的前提是错误本身被正确地产生出来，
	// 所以那一段必须单测。
	provider := sdktrace.NewTracerProvider()
	_, span := provider.Tracer("t").Start(context.Background(), "s")
	span.End()

	readOnly, ok := span.(sdktrace.ReadOnlySpan)
	if !ok {
		t.Fatalf("SDK 的 span 应实现 ReadOnlySpan，实际 %T", span)
	}
	err = exporter.ExportSpans(context.Background(), []sdktrace.ReadOnlySpan{readOnly})
	if err == nil {
		t.Fatal("采集端返回 400 时必须报错")
	}
	if !strings.Contains(err.Error(), "字段不认识") {
		t.Errorf("错误里应带上采集端的说明，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("错误里应带状态码，实际：%v", err)
	}
}

func stringAttribute(attributes []otlpKeyValue, key string) string {
	for _, kv := range attributes {
		if kv.Key == key && kv.Value.StringValue != nil {
			return *kv.Value.StringValue
		}
	}
	return ""
}

func intAttribute(attributes []otlpKeyValue, key string) string {
	for _, kv := range attributes {
		if kv.Key == key && kv.Value.IntValue != nil {
			return *kv.Value.IntValue
		}
	}
	return ""
}
