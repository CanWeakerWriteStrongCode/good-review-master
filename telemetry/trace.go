package telemetry

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// tracerName 是本程序在链路里上报的 instrumentation scope 名。
const tracerName = "good-review"

// tracer 用于创建 span。
//
// 在 InitTracing 之前取到的是全局 no-op tracer；一旦 SetTracerProvider 被调用，
// otel 会把**此前取出**的 tracer 一起换成真实实现（见 internal/global 的 setDelegate），
// 所以包级变量在这里是安全的、也是官方推荐的写法——不必为了拿 tracer 而到处传 provider。
//
// 未启用追踪时 StartSpan 什么都不做：业务代码可以无条件埋点，不用到处判开关。
var tracer = otel.Tracer(tracerName)

// StartSpan 开始一个 span，返回带 span 的 ctx 与 span 本身。
// 调用方必须 End（通常 defer span.End()）。
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return tracer.Start(ctx, name, opts...)
}

// InitTracing 按配置初始化链路追踪，返回关闭函数。
//
// endpoint 为空表示**关闭**：全局 TracerProvider 保持 no-op，StartSpan 产生的
// span 不采样、不上报，接近零成本。这是默认状态——追踪没有后端就是空转，
// 不该让每个用户都为一个自己没配的东西付出开销。
//
// serviceName 作为 resource 的 service.name 上报，用于在多服务面板上区分来源。
func InitTracing(endpoint, serviceName string) (func(context.Context) error, error) {
	noop := func(context.Context) error { return nil }
	if strings.TrimSpace(endpoint) == "" {
		return noop, nil
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewSchemaless(attribute.String("service.name", serviceName)),
	)
	if err != nil {
		// 合并失败（例如 SDK 换了 schema URL）不该让追踪整体不可用：退回只带服务名的 resource
		res = resource.NewSchemaless(attribute.String("service.name", serviceName))
	}

	exporter, err := newOTLPHTTPExporter(endpoint, res)
	if err != nil {
		return noop, fmt.Errorf("初始化链路追踪失败：%w", err)
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		// 全采样。本项目的"链路"就是「一条群消息 → 一次大模型调用」，
		// 量很小，采样等于把唯一的证据丢掉。真扛不住了再调比例。
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(1.0))),
	)
	otel.SetTracerProvider(provider)
	// 只装 W3C TraceContext：上游没传就现开一条链路。
	// 不引 baggage / b3 —— 本项目没有需要跨服务透传的业务上下文。
	otel.SetTextMapPropagator(propagation.TraceContext{})

	return provider.Shutdown, nil
}
