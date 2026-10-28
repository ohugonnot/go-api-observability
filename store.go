package main

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Invoice is the API resource. Customer and Email are personal data.
type Invoice struct {
	ID          string `json:"id"`
	Customer    string `json:"customer"`
	Email       string `json:"email"`
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency"`
}

// LogValue controls what slog prints for an Invoice: no personal data.
func (i Invoice) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("id", i.ID),
		slog.Int64("amount_cents", i.AmountCents),
		slog.String("currency", i.Currency),
	)
}

var ErrNotFound = errors.New("invoice not found")

// Store is an in-memory invoice repository.
type Store struct {
	tracer   trace.Tracer
	mu       sync.RWMutex
	invoices map[string]Invoice
	lastID   int
}

// NewStore returns a store seeded with one invoice (ID "1") for the demo.
func NewStore(tp trace.TracerProvider) *Store {
	return &Store{
		tracer: tp.Tracer("github.com/ohugonnot/go-api-observability/store"),
		invoices: map[string]Invoice{"1": {
			ID: "1", Customer: "ACME", Email: "billing@acme.example", AmountCents: 12000, Currency: "EUR",
		}},
		lastID: 1,
	}
}

// Get opens a child span of the request span carried by ctx.
func (s *Store) Get(ctx context.Context, id string) (Invoice, error) {
	_, span := s.tracer.Start(ctx, "store.Get", trace.WithAttributes(attribute.String("invoice.id", id)))
	defer span.End()

	s.mu.RLock()
	inv, ok := s.invoices[id]
	s.mu.RUnlock()
	span.SetAttributes(attribute.Bool("invoice.found", ok))
	if !ok {
		return Invoice{}, ErrNotFound
	}
	return inv, nil
}

func (s *Store) Create(ctx context.Context, inv Invoice) Invoice {
	_, span := s.tracer.Start(ctx, "store.Create")
	defer span.End()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastID++
	inv.ID = strconv.Itoa(s.lastID)
	s.invoices[inv.ID] = inv
	span.SetAttributes(attribute.String("invoice.id", inv.ID))
	return inv
}
