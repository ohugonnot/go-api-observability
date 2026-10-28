package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
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

const clientTraceID = "4bf92f3577b34da6a3ce929d0e0e4736"

// Public endpoint: the caller's traceparent becomes a link, not the parent.
func TestIncomingTraceparentBecomesALink(t *testing.T) {
	app := newTestApp(t)
	app.do("GET", "/invoices/1", "", "traceparent", "00-"+clientTraceID+"-00f067aa0ba902b7-01")

	server := app.span(t, "GET /invoices/{id}")
	if got := server.SpanContext().TraceID().String(); got == clientTraceID {
		t.Error("server span continued the client's trace; want a new root")
	}
	if server.Parent().IsValid() {
		t.Errorf("server span has parent %v, want a root span", server.Parent())
	}
	links := server.Links()
	if len(links) != 1 || links[0].SpanContext.TraceID().String() != clientTraceID {
		t.Errorf("links = %v, want one link to the client's trace %s", links, clientTraceID)
	}
}

// A client sending "not sampled" (-00) cannot hide a failing request.
func TestUnsampledTraceparentCannotHideA500(t *testing.T) {
	app := newTestApp(t)
	rec := app.do("GET", "/invoices/1?fail=1", "", "traceparent", "00-"+clientTraceID+"-00f067aa0ba902b7-00")
	if rec.Code != 500 {
		t.Fatalf("status = %d", rec.Code)
	}
	server := app.span(t, "GET /invoices/{id}")
	if !server.SpanContext().IsSampled() {
		t.Error("server span not sampled: the client opted the 500 out of tracing")
	}
	if ids := exemplarTraceIDs(t, app.reg); len(ids) != 1 || ids[0] != server.SpanContext().TraceID().String() {
		t.Errorf("exemplars = %v, want the server span's trace", ids)
	}
}

func TestUnmatchedRequestHasNoRouteAttribute(t *testing.T) {
	app := newTestApp(t)
	app.do("GET", "/nope", "")
	for _, s := range app.spans.Ended() {
		for _, a := range s.Attributes() {
			if a.Key == "http.route" {
				t.Errorf("span %q has http.route=%q on an unrouted request", s.Name(), a.Value.AsString())
			}
		}
	}
}

// SDK errors (failed exports...) reach our JSON log at ERROR level.
func TestOTelErrorsAreLogged(t *testing.T) {
	prevTP, prevProp, prevEH := otel.GetTracerProvider(), otel.GetTextMapPropagator(), otel.GetErrorHandler()
	t.Cleanup(func() {
		otel.SetTracerProvider(prevTP)
		otel.SetTextMapPropagator(prevProp)
		otel.SetErrorHandler(prevEH)
	})
	var buf strings.Builder
	setupOTel(newLogger(&buf), noop.NewTracerProvider())
	otel.Handle(errors.New("export failed"))
	if !strings.Contains(buf.String(), `"level":"ERROR","msg":"opentelemetry","error":"export failed"`) {
		t.Errorf("OTel error not logged as JSON ERROR: %q", buf.String())
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
