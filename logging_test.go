package main

import (
	"log/slog"
	"strings"
	"testing"
)

func TestLogsCarryTraceIDAndNoPersonalData(t *testing.T) {
	app := newTestApp(t)
	rec := app.do("POST", "/invoices",
		`{"customer":"Alice","email":"alice@example.com","amount_cents":4200,"currency":"EUR"}`)
	if rec.Code != 201 {
		t.Fatalf("status = %d", rec.Code)
	}
	server := app.span(t, "POST /invoices")
	wantTrace := server.SpanContext().TraceID().String()
	wantSpan := server.SpanContext().SpanID().String()

	lines := app.logLines(t)
	if len(lines) < 2 { // "invoice created" + access log
		t.Errorf("got %d log lines, want at least 2", len(lines))
	}
	for _, entry := range lines {
		if entry["trace_id"] != wantTrace || entry["span_id"] != wantSpan {
			t.Errorf("log %q: trace_id=%v span_id=%v, want %s %s",
				entry["msg"], entry["trace_id"], entry["span_id"], wantTrace, wantSpan)
		}
	}
	for _, secret := range []string{"alice@example.com", "Alice"} {
		if strings.Contains(app.logs.String(), secret) {
			t.Errorf("logs contain personal data %q", secret)
		}
	}
}

func TestServerErrorsAreLoggedAtErrorLevel(t *testing.T) {
	app := newTestApp(t)
	app.do("GET", "/invoices/1?fail=1", "")
	for _, entry := range app.logLines(t) {
		if entry["msg"] == "http request" {
			if entry["level"] != "ERROR" {
				t.Errorf("5xx access log level = %v, want ERROR", entry["level"])
			}
			return
		}
	}
	t.Error("no access log line")
}

// LogValue itself drops personal data, whatever the key it is logged under.
func TestInvoiceLogValue(t *testing.T) {
	inv := Invoice{ID: "7", Customer: "Alice", Email: "alice@example.com", AmountCents: 1, Currency: "EUR"}
	var keys []string
	for _, a := range inv.LogValue().Group() {
		keys = append(keys, a.Key)
	}
	if got := strings.Join(keys, ","); got != "id,amount_cents,currency" {
		t.Errorf("Invoice.LogValue keys = %s, want id,amount_cents,currency", got)
	}
}

func TestRedactSafetyNet(t *testing.T) {
	var buf strings.Builder
	newLogger(&buf).Info("oops",
		"email", "bob@example.com",
		"user_email", "carol@example.com",
		"Authorization", "Bearer x",
		"api_key", "k-123",
		"X-Api-Key", "k-456",
		"db_password", "hunter2",
		slog.Group("req", "session_token", "t-789"),
	)
	for _, secret := range []string{"bob@", "carol@", "Bearer", "k-123", "k-456", "hunter2", "t-789"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("sensitive value %q logged: %s", secret, buf.String())
		}
	}
}
