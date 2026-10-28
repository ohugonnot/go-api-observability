package main

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

type server struct {
	store *Store
	log   *slog.Logger
}

// newHandler wires routes and observability:
// otelhttp (server span) -> instrument (metrics, access log) -> mux -> handler.
// faults enables the ?delay= and ?fail= demo parameters (see demo.go).
func newHandler(log *slog.Logger, tp trace.TracerProvider, reg prometheus.Registerer, faults bool) http.Handler {
	s := &server{store: NewStore(tp), log: log}

	mux := http.NewServeMux()
	handle := func(pattern string, h http.HandlerFunc) {
		if faults {
			mux.Handle(pattern, demoFaults(h))
			return
		}
		mux.Handle(pattern, h)
	}
	handle("GET /invoices/{id}", s.getInvoice)
	handle("POST /invoices", s.createInvoice)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return otelhttp.NewHandler(instrument(mux, newHTTPMetrics(reg), log), "http.server",
		otelhttp.WithTracerProvider(tp),
		otelhttp.WithPropagators(propagation.TraceContext{}), // W3C traceparent
		otelhttp.WithFilter(func(r *http.Request) bool { return r.URL.Path != "/healthz" }),
	)
}

// Validation keeps raw client input out of spans, logs and memory.
var (
	validID       = regexp.MustCompile(`^[0-9]{1,18}$`)
	validCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
)

const maxTextLen = 256

func (s *server) getInvoice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		http.Error(w, "invoice not found", http.StatusNotFound) // nothing raw logged
		return
	}
	inv, err := s.store.Get(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		s.log.InfoContext(r.Context(), "invoice not found", "invoice_id", id)
		http.Error(w, "invoice not found", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

func (s *server) createInvoice(w http.ResponseWriter, r *http.Request) {
	var in Invoice
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	err := dec.Decode(&in)
	if err == nil {
		// Only whitespace may follow the object; this read can also hit the size limit.
		if extra := dec.Decode(&struct{}{}); extra != io.EOF {
			err = errors.Join(errors.New("data after the JSON object"), extra)
		}
	}
	if err != nil {
		if errors.As(err, new(*http.MaxBytesError)) {
			http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
			return
		}
		// Log the error class, never the request body.
		s.log.WarnContext(r.Context(), "invalid invoice payload", "error", "json decode failed")
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(in.Customer) == "" || len(in.Customer) > maxTextLen || len(in.Email) > maxTextLen ||
		in.AmountCents <= 0 || !validCurrency.MatchString(in.Currency) {
		http.Error(w, "customer (max 256 bytes), amount_cents > 0 and an ISO currency like EUR are required",
			http.StatusUnprocessableEntity)
		return
	}

	inv := s.store.Create(r.Context(), in)
	s.log.InfoContext(r.Context(), "invoice created", "invoice", inv) // Invoice.LogValue drops PII
	w.Header().Set("Location", "/invoices/"+inv.ID)
	writeJSON(w, http.StatusCreated, inv)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
