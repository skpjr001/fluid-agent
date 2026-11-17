package main

import (
	"context"
	"net/http"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

var (
	tracer     trace.Tracer = otel.Tracer("fluid-agent")
	propagator              = propagation.TraceContext{}
)

// setupTracing enables OTLP/HTTP trace export when telemetry.otlp_endpoint
// (or OTEL_EXPORTER_OTLP_ENDPOINT) is set. It returns a shutdown func.
func setupTracing(ctx context.Context, cfg TelemetryConfig) (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagator)
	endpoint := cfg.OTLPEndpoint
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(endpoint + "/v1/traces")}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	res := resource.NewSchemaless(
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.version", version),
	)
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRatio))),
	)
	otel.SetTracerProvider(tp)
	tracer = tp.Tracer("fluid-agent")
	return tp.Shutdown, nil
}

func traceIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// extractTrace continues a trace started by the caller (traceparent header).
func extractTrace(ctx context.Context, h http.Header) context.Context {
	return propagator.Extract(ctx, propagation.HeaderCarrier(h))
}

func injectTrace(ctx context.Context, h http.Header) {
	propagator.Inject(ctx, propagation.HeaderCarrier(h))
}

// traceparentOf serializes the span context so a later background delivery
// can continue the same trace.
func traceparentOf(ctx context.Context) string {
	h := http.Header{}
	injectTrace(ctx, h)
	return h.Get("Traceparent")
}

func contextFromTraceparent(tp string) context.Context {
	if tp == "" {
		return context.Background()
	}
	h := http.Header{}
	h.Set("Traceparent", tp)
	return extractTrace(context.Background(), h)
}

func endSpan(span trace.Span, status int, err error) {
	if status > 0 {
		span.SetAttributes(attribute.Int("http.response.status_code", status))
	}
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	} else if status >= 500 {
		span.SetStatus(codes.Error, http.StatusText(status))
	}
	span.End()
}
