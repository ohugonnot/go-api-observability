package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Three different invoice IDs must produce one series per route and status,
// not one series per ID: the label is the route pattern.
func TestRequestCounterUsesRoutePattern(t *testing.T) {
	app := newTestApp(t)
	app.do("GET", "/invoices/1", "")
	app.do("GET", "/invoices/1", "")
	app.do("GET", "/invoices/42", "") // 404
	app.do("GET", "/does/not/exist", "")

	want := `
# HELP http_requests_total HTTP requests by method, route pattern and status code.
# TYPE http_requests_total counter
http_requests_total{code="200",method="GET",route="/invoices/{id}"} 2
http_requests_total{code="404",method="GET",route="/invoices/{id}"} 1
http_requests_total{code="404",method="GET",route="unmatched"} 1
`
	if err := testutil.GatherAndCompare(app.reg, strings.NewReader(want), "http_requests_total"); err != nil {
		t.Error(err)
	}
}

func TestUnknownMethodIsBounded(t *testing.T) {
	app := newTestApp(t)
	app.do("BREW", "/invoices/1", "")

	want := `
# HELP http_requests_total HTTP requests by method, route pattern and status code.
# TYPE http_requests_total counter
http_requests_total{code="405",method="OTHER",route="unmatched"} 1
`
	if err := testutil.GatherAndCompare(app.reg, strings.NewReader(want), "http_requests_total"); err != nil {
		t.Error(err)
	}
}

// A panicking handler must still show up in the RED metrics as a 500.
func TestPanicIsCountedAs500(t *testing.T) {
	reg := prometheus.NewRegistry()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	h := instrument(mux, newHTTPMetrics(reg), slog.New(slog.DiscardHandler))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/boom", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	want := `
# HELP http_requests_total HTTP requests by method, route pattern and status code.
# TYPE http_requests_total counter
http_requests_total{code="500",method="GET",route="/boom"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "http_requests_total"); err != nil {
		t.Error(err)
	}
	if n := testutil.CollectAndCount(reg, "http_request_duration_seconds"); n != 1 {
		t.Errorf("duration series = %d, want 1", n)
	}
}

// Health probes are not requests of the SLI: no series at all.
func TestHealthzIsNotCounted(t *testing.T) {
	app := newTestApp(t)
	for range 3 {
		if code := app.do("GET", "/healthz", "").Code; code != http.StatusOK {
			t.Fatalf("/healthz status = %d", code)
		}
	}
	if n := testutil.CollectAndCount(app.reg, "http_requests_total"); n != 0 {
		t.Errorf("/healthz produced %d http_requests_total series, want 0", n)
	}
	if app.logs.Len() != 0 {
		t.Errorf("/healthz logged: %s", app.logs.String())
	}
}

// A panic after a partial write: the span is marked failed (otelhttp cannot,
// the panic skips its status code), so the Collector keeps the trace that
// the exemplar points to.
func TestPanicAfterWriteMarksSpanError(t *testing.T) {
	app := newTestApp(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	tp, err := newTracerProvider(t.Context(), 1, sdktrace.WithSpanProcessor(app.spans))
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /half", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	})
	reg := prometheus.NewRegistry()
	h := observe(mux, newLogger(app.logs), tp, newHTTPMetrics(reg))
	if p := serveRecovering(h, httptest.NewRecorder(), httptest.NewRequest("GET", "/half", nil)); p != http.ErrAbortHandler {
		t.Fatalf("recovered %v, want http.ErrAbortHandler", p)
	}

	var server sdktrace.ReadOnlySpan
	for _, s := range app.spans.Ended() {
		if s.SpanKind() == trace.SpanKindServer {
			server = s
		}
	}
	if server == nil {
		t.Fatal("no server span")
	}
	if server.Status().Code != codes.Error {
		t.Errorf("server span status = %v, want Error", server.Status().Code)
	}
	// The SDK records the panic that crosses span.End, but that one is
	// http.ErrAbortHandler: only our RecordError keeps the original value.
	var recorded bool
	for _, e := range server.Events() {
		for _, a := range e.Attributes {
			if e.Name == "exception" && a.Key == "exception.message" && strings.Contains(a.Value.AsString(), "boom") {
				recorded = true
			}
		}
	}
	if !recorded {
		t.Errorf("original panic value not recorded on the span: %v", server.Events())
	}
	if ids := exemplarTraceIDs(t, reg); len(ids) != 1 || ids[0] != server.SpanContext().TraceID().String() {
		t.Errorf("exemplars = %v, want the failed span's trace", ids)
	}
}

// exemplarTraceIDs returns the trace_id of every exemplar on the duration histogram.
func exemplarTraceIDs(t *testing.T, reg *prometheus.Registry) []string {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, f := range families {
		if f.GetName() != "http_request_duration_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, b := range m.GetHistogram().GetBucket() {
				for _, l := range b.GetExemplar().GetLabel() {
					if l.GetName() == "trace_id" {
						ids = append(ids, l.GetValue())
					}
				}
			}
		}
	}
	return ids
}

// Exemplars only for requests the Collector always keeps: a fast 200 has
// none, a 500 has one with its trace ID.
func TestExemplarsOnlyForKeptTraces(t *testing.T) {
	app := newTestApp(t)
	app.do("GET", "/invoices/1", "")
	if ids := exemplarTraceIDs(t, app.reg); len(ids) != 0 {
		t.Fatalf("fast 200 got exemplars %v, want none", ids)
	}

	app.do("GET", "/invoices/1?fail=1", "")
	ids := exemplarTraceIDs(t, app.reg)
	var traceID string
	for _, s := range app.spans.Ended() {
		if s.Name() == "GET /invoices/{id}" {
			traceID = s.SpanContext().TraceID().String() // the last one is the 500
		}
	}
	if len(ids) != 1 || ids[0] != traceID {
		t.Errorf("exemplars = %v, want [%s] from the 500", ids, traceID)
	}
}

// Exemplars only exist in the OpenMetrics format.
func TestMetricsEndpointServesExemplars(t *testing.T) {
	app := newTestApp(t)
	app.do("GET", "/invoices/1?fail=1", "")

	req := httptest.NewRequest("GET", "/metrics", nil)
	req.Header.Set("Accept", "application/openmetrics-text; version=1.0.0")
	rec := httptest.NewRecorder()
	metricsHandler(app.reg).ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "# {trace_id=") {
		t.Errorf("no exemplar in /metrics (OpenMetrics) output:\n%.500s", rec.Body.String())
	}
}

func TestSlowRequestGetsExemplar(t *testing.T) {
	app := newTestApp(t)
	app.do("GET", "/invoices/1?delay="+slowThreshold.String(), "")
	if ids := exemplarTraceIDs(t, app.reg); len(ids) != 1 {
		t.Errorf("slow request exemplars = %v, want 1", ids)
	}
}

// serveRecovering calls h and returns what it panicked with, if anything.
func serveRecovering(h http.Handler, rec *httptest.ResponseRecorder, req *http.Request) (p any) {
	defer func() { p = recover() }()
	h.ServeHTTP(rec, req)
	return nil
}

// http.ErrAbortHandler is net/http's own "drop this connection" signal: it is
// re-panicked untouched, neither counted nor logged as a 500.
func TestAbortHandlerIsRepanickedUncounted(t *testing.T) {
	reg := prometheus.NewRegistry()
	var logs strings.Builder
	mux := http.NewServeMux()
	mux.HandleFunc("GET /abort", func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })
	h := instrument(mux, newHTTPMetrics(reg), newLogger(&logs))

	p := serveRecovering(h, httptest.NewRecorder(), httptest.NewRequest("GET", "/abort", nil))
	if p != http.ErrAbortHandler {
		t.Errorf("recovered %v, want http.ErrAbortHandler re-panicked", p)
	}
	if n := testutil.CollectAndCount(reg, "http_requests_total"); n != 0 {
		t.Errorf("abort counted in %d series, want 0", n)
	}
	if strings.Contains(logs.String(), "panic recovered") {
		t.Errorf("abort logged as a panic: %s", logs.String())
	}
}

// Once bytes are out, a 500 cannot be sent: the request is counted as a 500,
// the connection is aborted, and no second status line or error text is written.
func TestPanicAfterWriteAbortsConnection(t *testing.T) {
	reg := prometheus.NewRegistry()
	var logs strings.Builder
	mux := http.NewServeMux()
	mux.HandleFunc("GET /half", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("partial"))
		panic("boom")
	})
	h := instrument(mux, newHTTPMetrics(reg), newLogger(&logs))

	rec := httptest.NewRecorder()
	p := serveRecovering(h, rec, httptest.NewRequest("GET", "/half", nil))
	if p != http.ErrAbortHandler {
		t.Errorf("recovered %v, want http.ErrAbortHandler to abort the connection", p)
	}
	if rec.Code != http.StatusOK || rec.Body.String() != "partial" {
		t.Errorf("response = %d %q, want the untouched partial 200", rec.Code, rec.Body.String())
	}
	want := `
# HELP http_requests_total HTTP requests by method, route pattern and status code.
# TYPE http_requests_total counter
http_requests_total{code="500",method="GET",route="/half"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "http_requests_total"); err != nil {
		t.Error(err)
	}
	for _, s := range []string{`"panic_type":"string"`, `"stack":"goroutine`} {
		if !strings.Contains(logs.String(), s) {
			t.Errorf("panic log lacks %s: %s", s, logs.String())
		}
	}
}
