package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// 本文件手写了一个 OTLP/HTTP 的 span 导出器。
//
// 为什么不用官方的 go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp：
// 它会连带拖进 gRPC + genproto + protobuf + grpc-gateway + backoff 一整棵依赖子树，
// 而当前 grpc/genproto 的版本要求 go >= 1.26（本仓库是 1.25）。
// 于是只有两条路：为一棵子树长期钉死版本，或者抬高整个主模块的 go 指令——
// 两者都不该为一个**默认关闭**的功能付。
//
// 代价是这里约 150 行序列化代码；收益是追踪这条链路只依赖 otel 核心与 sdk。
// 协议侧没有打折：OTLP/HTTP 规范明确定义了 protobuf 的 JSON 编码，
// Jaeger / OTel Collector / Tempo 在 /v1/traces 上收的就是这个格式，
// 所以 runtime.otlp_endpoint 指过去依然是一个标准的 OTLP 端点。

// otlpTracesPath OTLP/HTTP 的 traces 路径（规范固定，不可配）。
const otlpTracesPath = "/v1/traces"

// otlpHTTPExporter 把 span 以 OTLP/HTTP + JSON 上报。
type otlpHTTPExporter struct {
	url      string
	resource *resource.Resource
	client   *http.Client
}

// newOTLPHTTPExporter 解析端点。endpoint 允许带或不带 /v1/traces 后缀：
// 用户按惯例写 http://localhost:4318（OTEL_EXPORTER_OTLP_ENDPOINT 的写法）即可，
// 已经写全了也不会被拼成 .../v1/traces/v1/traces。
func newOTLPHTTPExporter(endpoint string, res *resource.Resource) (*otlpHTTPExporter, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return nil, fmt.Errorf("otlp_endpoint 为空")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("otlp_endpoint 必须是合法的 http(s) 地址（如 http://localhost:4318），当前是 %q", endpoint)
	}

	endpoint = strings.TrimSuffix(endpoint, "/")
	if !strings.HasSuffix(endpoint, otlpTracesPath) {
		endpoint += otlpTracesPath
	}
	return &otlpHTTPExporter{
		url:      endpoint,
		resource: res,
		client:   &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// ExportSpans 实现 sdktrace.SpanExporter。
//
// 返回错误时 BatchSpanProcessor 会记一条日志并丢掉这批 span（不会重试、不会阻塞业务），
// 所以上报失败只会让链路缺一段，不会影响机器人本身——这正是追踪应有的失败姿态。
func (e *otlpHTTPExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	if len(spans) == 0 {
		return nil
	}
	body, err := json.Marshal(e.buildRequest(spans))
	if err != nil {
		return fmt.Errorf("OTLP 请求序列化失败: %w", err)
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := e.client.Do(request)
	if err != nil {
		return fmt.Errorf("上报 span 失败: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode/100 != 2 {
		// 采集端会在响应体里说明拒绝原因（如 JSON 字段不认识），带上它才有得查
		snippet, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("OTLP 端点 %s 返回 %d: %s", e.url, response.StatusCode, strings.TrimSpace(string(snippet)))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	return nil
}

// Shutdown 实现 sdktrace.SpanExporter。这里没有守护 goroutine，关掉空闲连接即可。
func (e *otlpHTTPExporter) Shutdown(context.Context) error {
	e.client.CloseIdleConnections()
	return nil
}

// buildRequest 把一批 span 组装成 OTLP 请求体。
//
// 一个进程只有一份 resource（同一次 InitTracing 里构造），所以整批 span 属于同一个
// resourceSpans 条目；scopeSpans 按 instrumentation scope 的名字分组——
// 本项目只有一个 scope（good-review），但按规范分组，将来多一个 tracer 也不会串。
func (e *otlpHTTPExporter) buildRequest(spans []sdktrace.ReadOnlySpan) otlpTraceRequest {
	byScope := make(map[string]*otlpScopeSpans)
	var scopeOrder []string

	for _, span := range spans {
		name := span.InstrumentationScope().Name
		group, exists := byScope[name]
		if !exists {
			group = &otlpScopeSpans{Scope: otlpScope{Name: name}}
			byScope[name] = group
			scopeOrder = append(scopeOrder, name)
		}
		group.Spans = append(group.Spans, toOTLPSpan(span))
	}

	scopeSpans := make([]otlpScopeSpans, 0, len(scopeOrder))
	for _, name := range scopeOrder {
		scopeSpans = append(scopeSpans, *byScope[name])
	}

	return otlpTraceRequest{
		ResourceSpans: []otlpResourceSpans{{
			Resource:   otlpResource{Attributes: toOTLPAttributes(e.resourceAttributes())},
			ScopeSpans: scopeSpans,
		}},
	}
}

func (e *otlpHTTPExporter) resourceAttributes() []attribute.KeyValue {
	if e.resource == nil {
		return nil
	}
	return e.resource.Attributes()
}

// toOTLPSpan 转换单个 span。
func toOTLPSpan(span sdktrace.ReadOnlySpan) otlpSpan {
	spanContext := span.SpanContext()
	out := otlpSpan{
		TraceID:           spanContext.TraceID().String(),
		SpanID:            spanContext.SpanID().String(),
		Name:              span.Name(),
		Kind:              int(span.SpanKind()),
		StartTimeUnixNano: strconv.FormatInt(span.StartTime().UnixNano(), 10),
		EndTimeUnixNano:   strconv.FormatInt(span.EndTime().UnixNano(), 10),
		Attributes:        toOTLPAttributes(span.Attributes()),
	}
	if parent := span.Parent(); parent.HasSpanID() {
		out.ParentSpanID = parent.SpanID().String()
	}
	if status := span.Status(); status.Code != codes.Unset {
		out.Status = &otlpStatus{Code: otlpStatusCode(status.Code), Message: status.Description}
	}
	// Kind 直接取 int 转换：SDK 的 trace.SpanKind 与 OTLP 的 SpanKind 取值恰好一一对应
	// （Unspecified=0..Consumer=5）。这一行依赖那个巧合，所以在此写明——
	// 哪天 SDK 改了顺序，看这里就能想起来该显式映射了。
	return out
}

// otlpStatusCode 把 SDK 的 codes.Code 映射到 OTLP 的 Status.Code。
// 两套枚举**顺序不同**，必须显式映射：
//
//	codes.Code: Unset=0, Error=1, Ok=2
//	OTLP:       UNSET=0, OK=1,    ERROR=2
func otlpStatusCode(code codes.Code) int {
	switch code {
	case codes.Ok:
		return 1
	case codes.Error:
		return 2
	default:
		return 0
	}
}

// toOTLPAttributes 转换属性列表。
func toOTLPAttributes(attributes []attribute.KeyValue) []otlpKeyValue {
	if len(attributes) == 0 {
		return nil
	}
	out := make([]otlpKeyValue, 0, len(attributes))
	for _, kv := range attributes {
		out = append(out, otlpKeyValue{Key: string(kv.Key), Value: toOTLPValue(kv.Value)})
	}
	return out
}

// toOTLPValue 转换单个属性值。OTLP/JSON 里 int64 是**字符串**（JSON number 装不下 int64）。
func toOTLPValue(value attribute.Value) otlpValue {
	switch value.Type() {
	case attribute.INT64:
		text := strconv.FormatInt(value.AsInt64(), 10)
		return otlpValue{IntValue: &text}
	case attribute.FLOAT64:
		number := value.AsFloat64()
		return otlpValue{DoubleValue: &number}
	case attribute.BOOL:
		flag := value.AsBool()
		return otlpValue{BoolValue: &flag}
	default:
		// 字符串与其余类型（含数组、KV 列表这类复合类型）一律降级成字符串。
		// 本项目的 span 属性全是标量，为一类用不到的复合类型去实现 arrayValue/kvlistValue
		// 只会增加出错面；降级至少不丢信息，也不会因为一个属性序列化失败丢整批 span。
		text := value.Emit()
		return otlpValue{StringValue: &text}
	}
}

// 以下结构体对应 OTLP/HTTP JSON 编码的 protobuf 结构（字段名严格按规范，
// 用 lowerCamelCase、int64 用字符串）。

type otlpTraceRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            *otlpStatus    `json:"status,omitempty"`
}

type otlpStatus struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}
