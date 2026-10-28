package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

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

func (s *server) getInvoice(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
	if err := dec.Decode(&in); err != nil {
		// Log the error class, never the request body.
		s.log.WarnContext(r.Context(), "invalid invoice payload", "error", "json decode failed")
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if in.Customer == "" || in.AmountCents <= 0 || len(in.Currency) != 3 {
		http.Error(w, "customer, amount_cents > 0 and a 3-letter currency are required", http.StatusUnprocessableEntity)
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
