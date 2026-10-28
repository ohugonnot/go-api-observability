package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
