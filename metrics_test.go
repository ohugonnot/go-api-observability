package main

import (
	"strings"
	"testing"

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

func TestDurationHistogramHasExemplar(t *testing.T) {
	app := newTestApp(t)
	app.do("GET", "/invoices/1", "")
	traceID := app.span(t, "GET /invoices/{id}").SpanContext().TraceID().String()

	families, err := app.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range families {
		if f.GetName() != "http_request_duration_seconds" {
			continue
		}
		for _, b := range f.GetMetric()[0].GetHistogram().GetBucket() {
			for _, l := range b.GetExemplar().GetLabel() {
				if l.GetName() == "trace_id" && l.GetValue() == traceID {
					return
				}
			}
		}
	}
	t.Errorf("no exemplar with trace_id=%s on http_request_duration_seconds", traceID)
}
