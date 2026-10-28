package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// newSampler: root spans keep `ratio` of traces, child spans follow the
// caller's decision (the sampled flag of the incoming traceparent).
func newSampler(ratio float64) sdktrace.Sampler {
	return sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))
}

// newTracerProvider exports over OTLP when OTEL_EXPORTER_OTLP_ENDPOINT is set.
// Extra options let tests plug in-memory processors.
func newTracerProvider(ctx context.Context, ratio float64, extra ...sdktrace.TracerProviderOption) (*sdktrace.TracerProvider, error) {
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", envOr("OTEL_SERVICE_NAME", "invoice-api")),
	))
	if err != nil {
		return nil, err
	}
	// Attribute values come partly from clients (url.path, user agent...): cap them.
	limits := sdktrace.NewSpanLimits()
	limits.AttributeValueLengthLimit = 256

	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res),
		sdktrace.WithSampler(newSampler(ratio)),
		sdktrace.WithRawSpanLimits(limits),
	}
	// Without a Collector, spans are still created (trace_id in logs) but not exported.
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" {
		exp, err := otlptracehttp.New(ctx) // reads the OTEL_EXPORTER_OTLP_* variables
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exp))
	}
	return sdktrace.NewTracerProvider(append(opts, extra...)...), nil
}

// sampleRatio parses TRACE_SAMPLE_RATIO; empty means 1 (keep everything).
func sampleRatio(s string) (float64, error) {
	if s == "" {
		return 1, nil
	}
	r, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(r) || r < 0 || r > 1 {
		return 0, fmt.Errorf("TRACE_SAMPLE_RATIO must be a number between 0 and 1, got %q", s)
	}
	return r, nil
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// spanTracer returns a tracer from the provider that created the request span.
func spanTracer(r *http.Request) trace.Tracer {
	return trace.SpanFromContext(r.Context()).TracerProvider().Tracer("github.com/ohugonnot/go-api-observability")
}
