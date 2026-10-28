package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// testApp is the real handler wired to in-memory telemetry sinks.
type testApp struct {
	handler http.Handler
	spans   *tracetest.SpanRecorder
	reg     *prometheus.Registry
	logs    *bytes.Buffer
}

func newTestApp(t *testing.T) *testApp {
	t.Helper()
	spans := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans))
	t.Cleanup(func() { _ = tp.Shutdown(t.Context()) })

	app := &testApp{spans: spans, reg: prometheus.NewRegistry(), logs: &bytes.Buffer{}}
	app.handler = newHandler(newLogger(app.logs), tp, app.reg, true)
	return app
}

func (a *testApp) do(method, target, body string, header ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	a.handler.ServeHTTP(rec, req)
	return rec
}

// span returns the ended span with the given name.
func (a *testApp) span(t *testing.T, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	for _, s := range a.spans.Ended() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("no span named %q", name)
	return nil
}
