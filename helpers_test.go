package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	return newTestAppWith(t, true)
}

// newTestAppWith uses the production TracerProvider (sampler, span limits)
// with a span recorder instead of the OTLP exporter.
func newTestAppWith(t *testing.T, faults bool) *testApp {
	t.Helper()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	spans := tracetest.NewSpanRecorder()
	tp, err := newTracerProvider(t.Context(), 1, sdktrace.WithSpanProcessor(spans))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	app := &testApp{spans: spans, reg: prometheus.NewRegistry(), logs: &bytes.Buffer{}}
	app.handler = newHandler(newLogger(app.logs), tp, app.reg, faults)
	return app
}

// logLines decodes the JSON log lines.
func (a *testApp) logLines(t *testing.T) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(a.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %s", line)
		}
		out = append(out, entry)
	}
	return out
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
