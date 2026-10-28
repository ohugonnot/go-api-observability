package main

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

func TestServerSpanHasRouteAndStoreChild(t *testing.T) {
	app := newTestApp(t)
	app.do("GET", "/invoices/1", "")

	server := app.span(t, "GET /invoices/{id}") // named after the pattern, not the URL
	if server.SpanKind() != trace.SpanKindServer {
		t.Errorf("kind = %v, want server", server.SpanKind())
	}
	if !hasAttr(server.Attributes(), attribute.String("http.route", "/invoices/{id}")) {
		t.Errorf("server span has no http.route=/invoices/{id}: %v", server.Attributes())
	}

	store := app.span(t, "store.Get")
	if store.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Errorf("store.Get parent = %s, want server span %s", store.Parent().SpanID(), server.SpanContext().SpanID())
	}
	if store.SpanContext().TraceID() != server.SpanContext().TraceID() {
		t.Error("store.Get is not in the same trace as the server span")
	}
}

func TestIncomingTraceparentIsContinued(t *testing.T) {
	app := newTestApp(t)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	app.do("GET", "/invoices/1", "", "traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")

	server := app.span(t, "GET /invoices/{id}")
	if got := server.SpanContext().TraceID().String(); got != traceID {
		t.Errorf("trace ID = %s, want the caller's %s", got, traceID)
	}
	if !server.Parent().IsRemote() {
		t.Error("server span parent should be the remote caller")
	}
}

func TestHealthzIsNotTraced(t *testing.T) {
	app := newTestApp(t)
	app.do("GET", "/healthz", "")
	if n := len(app.spans.Ended()); n != 0 {
		t.Errorf("/healthz produced %d spans, want 0", n)
	}
}

// Client-controlled values (url.path here) are truncated to 256 bytes.
func TestSpanAttributesAreCapped(t *testing.T) {
	app := newTestApp(t)
	app.do("GET", "/"+strings.Repeat("a", 5000), "")
	for _, s := range app.spans.Ended() {
		for _, a := range s.Attributes() {
			if len(a.Value.String()) > 256 {
				t.Errorf("span %q attribute %s is %d bytes", s.Name(), a.Key, len(a.Value.String()))
			}
		}
	}
}

func TestSampler(t *testing.T) {
	sampledParent := trace.ContextWithRemoteSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: trace.TraceID{1}, SpanID: trace.SpanID{1}, TraceFlags: trace.FlagsSampled, Remote: true,
	}))
	decide := func(ratio float64, ctx context.Context) sdktrace.SamplingDecision {
		return newSampler(ratio).ShouldSample(sdktrace.SamplingParameters{
			ParentContext: ctx, TraceID: trace.TraceID{0xff}, Name: "x",
		}).Decision
	}
	if d := decide(0, sampledParent); d != sdktrace.RecordAndSample {
		t.Errorf("ratio 0 with a sampled parent: %v, want RecordAndSample (ParentBased)", d)
	}
	if d := decide(0, context.Background()); d != sdktrace.Drop {
		t.Errorf("ratio 0 without parent: %v, want Drop", d)
	}
	if d := decide(1, context.Background()); d != sdktrace.RecordAndSample {
		t.Errorf("ratio 1 without parent: %v, want RecordAndSample", d)
	}
}

func TestTracerProviderUsesRatio(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	tp, err := newTracerProvider(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer tp.Shutdown(context.Background())
	_, span := tp.Tracer("t").Start(context.Background(), "root")
	defer span.End()
	if !span.SpanContext().IsSampled() {
		t.Error("ratio 1: root span should be sampled")
	}
}

// fakeExporter counts spans; unlike tracetest.InMemoryExporter it keeps
// them after Shutdown.
type fakeExporter struct {
	mu    sync.Mutex
	spans int
}

func (e *fakeExporter) ExportSpans(_ context.Context, s []sdktrace.ReadOnlySpan) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.spans += len(s)
	return nil
}
func (e *fakeExporter) Shutdown(context.Context) error { return nil }

// shutdown must flush the batcher: otherwise the last spans are lost.
func TestShutdownFlushesSpans(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	exp := &fakeExporter{}
	tp, err := newTracerProvider(t.Context(), 1, sdktrace.WithBatcher(exp))
	if err != nil {
		t.Fatal(err)
	}
	_, span := tp.Tracer("t").Start(context.Background(), "last request")
	span.End()

	if err := shutdown(tp, &http.Server{}); err != nil {
		t.Fatal(err)
	}
	exp.mu.Lock()
	defer exp.mu.Unlock()
	if exp.spans != 1 {
		t.Errorf("exported %d spans after shutdown, want 1", exp.spans)
	}
}

func TestSampleRatio(t *testing.T) {
	for in, want := range map[string]float64{"": 1, "0.1": 0.1, "0": 0, "1": 1} {
		if got, err := sampleRatio(in); err != nil || got != want {
			t.Errorf("sampleRatio(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"abc", "-1", "1.5", "NaN", "nan", "Inf"} {
		if r, err := sampleRatio(in); err == nil {
			t.Errorf("sampleRatio(%q) = %v, should fail", in, r)
		}
	}
}

func hasAttr(attrs []attribute.KeyValue, want attribute.KeyValue) bool {
	for _, a := range attrs {
		if a == want {
			return true
		}
	}
	return false
}
