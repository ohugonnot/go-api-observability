package main

import (
	"bufio"
	"encoding/json"
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

	logs := app.logs.String()
	var lines int
	sc := bufio.NewScanner(strings.NewReader(logs))
	for sc.Scan() {
		var entry map[string]any
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			t.Fatalf("log line is not JSON: %s", sc.Text())
		}
		lines++
		if entry["trace_id"] != wantTrace || entry["span_id"] != wantSpan {
			t.Errorf("log %q: trace_id=%v span_id=%v, want %s %s",
				entry["msg"], entry["trace_id"], entry["span_id"], wantTrace, wantSpan)
		}
	}
	if lines < 2 { // "invoice created" + access log
		t.Errorf("got %d log lines, want at least 2", lines)
	}
	for _, secret := range []string{"alice@example.com", "Alice"} {
		if strings.Contains(logs, secret) {
			t.Errorf("logs contain personal data %q", secret)
		}
	}
}

func TestRedactSafetyNet(t *testing.T) {
	var buf strings.Builder
	newLogger(&buf).Info("oops", "email", "bob@example.com", "Authorization", "Bearer x")
	if strings.Contains(buf.String(), "bob@example.com") || strings.Contains(buf.String(), "Bearer") {
		t.Errorf("sensitive value logged: %s", buf.String())
	}
}
