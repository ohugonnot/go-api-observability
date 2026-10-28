package main

import (
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// httpMetrics implements the RED method: Rate and Errors (counter), Duration (histogram).
type httpMetrics struct {
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newHTTPMetrics(reg prometheus.Registerer) *httpMetrics {
	m := &httpMetrics{
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "HTTP requests by method, route pattern and status code.",
		}, []string{"method", "route", "code"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "HTTP request duration by method and route pattern.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route"}),
	}
	reg.MustRegister(m.requests, m.duration)
	return m
}

// newRegistry returns a registry with process and Go runtime metrics.
// The Go collector reads runtime/metrics; the rules select GC, memory and scheduler series.
func newRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(collectors.WithGoCollectorRuntimeMetrics(
			collectors.MetricsGC, collectors.MetricsMemory, collectors.MetricsScheduler,
		)),
	)
	return reg
}

func metricsHandler(reg *prometheus.Registry) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{
		EnableOpenMetrics: true, // exemplars are only exposed in the OpenMetrics format
	}))
	return mux
}

// slowThreshold must match the "slow" latency policy (threshold_ms: 500) in
// otel-collector/config.yaml: such traces are always kept by the Collector.
const slowThreshold = 500 * time.Millisecond

// instrument records RED metrics and one access log line per request.
// It must receive the same *http.Request as the mux: the mux sets r.Pattern on it.
func instrument(next http.Handler, m *httpMetrics, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			// A panicking handler is still a request: count it as a 500.
			p := recover()
			if p == http.ErrAbortHandler {
				panic(p) // deliberate abort: let net/http handle it
			}
			if p != nil {
				log.ErrorContext(r.Context(), "panic recovered", "panic", fmt.Sprint(p))
				if !rec.wrote {
					http.Error(rec, "internal server error", http.StatusInternalServerError)
				}
				rec.status = http.StatusInternalServerError
			}
			record(r, rec.status, time.Since(start), m, log)
		}()
		next.ServeHTTP(rec, r)
	})
}

func record(r *http.Request, status int, elapsed time.Duration, m *httpMetrics, log *slog.Logger) {
	// Label with the route pattern ("/invoices/{id}"), never r.URL.Path ("/invoices/42"):
	// one series per route instead of one per invoice.
	route, method := routeLabel(r), methodLabel(r.Method)
	m.requests.WithLabelValues(method, route, strconv.Itoa(status)).Inc()

	obs := m.duration.WithLabelValues(method, route)
	sc := trace.SpanContextFromContext(r.Context())
	// Exemplar only for requests the Collector always keeps (5xx or slow):
	// a fast 200 would mostly link to a trace dropped by tail sampling.
	if eo, ok := obs.(prometheus.ExemplarObserver); ok && sc.IsSampled() &&
		(status >= 500 || elapsed >= slowThreshold) {
		eo.ObserveWithExemplar(elapsed.Seconds(), prometheus.Labels{"trace_id": sc.TraceID().String()})
	} else {
		obs.Observe(elapsed.Seconds())
	}

	trace.SpanFromContext(r.Context()).SetAttributes(attribute.String("http.route", route))
	level := slog.LevelInfo
	if status >= 500 {
		level = slog.LevelError
	}
	log.Log(r.Context(), level, "http request",
		"method", method, "route", route, "status", status,
		"duration_ms", float64(elapsed.Microseconds())/1000)
}

func routeLabel(r *http.Request) string {
	if r.Pattern == "" {
		return "unmatched" // 404/405: bounded, whatever the URL
	}
	if _, path, ok := strings.Cut(r.Pattern, " "); ok {
		return path // "GET /invoices/{id}" -> "/invoices/{id}"
	}
	return r.Pattern
}

// methodLabel bounds the method label: clients can send arbitrary methods.
func methodLabel(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions:
		return m
	}
	return "OTHER"
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

// WriteHeader keeps the first final status (1xx are informational).
func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote && code >= 200 {
		r.status, r.wrote = code, true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	r.wrote = true
	return r.ResponseWriter.Write(b)
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
