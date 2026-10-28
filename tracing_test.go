package main

import (
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

func TestServerSpanHasRouteAndStoreChild(t *testing.T) {
	app := newTestApp(t)
	app.do("GET", "/invoices/1", "")

	server := app.span(t, "GET /invoices/{id}") // named after the pattern, not the URL
	if server.SpanKind() != trace.SpanKindServer {
		t.Errorf("kind = %v, want server", server.SpanKind())
	}
	if !hasAttr(server.Attributes(), attribute.String("http.route", "/invoices/{id}")) {
		t.Errorf("server span has no http.route=/invoices/{id}: %v", server.Attributes())
	}

	store := app.span(t, "store.Get")
	if store.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Errorf("store.Get parent = %s, want server span %s", store.Parent().SpanID(), server.SpanContext().SpanID())
	}
	if store.SpanContext().TraceID() != server.SpanContext().TraceID() {
		t.Error("store.Get is not in the same trace as the server span")
	}
}

func TestIncomingTraceparentIsContinued(t *testing.T) {
	app := newTestApp(t)
	const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"
	app.do("GET", "/invoices/1", "", "traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")

	server := app.span(t, "GET /invoices/{id}")
	if got := server.SpanContext().TraceID().String(); got != traceID {
		t.Errorf("trace ID = %s, want the caller's %s", got, traceID)
	}
	if !server.Parent().IsRemote() {
		t.Error("server span parent should be the remote caller")
	}
}

func TestSampleRatio(t *testing.T) {
	for in, want := range map[string]float64{"": 1, "0.1": 0.1, "0": 0} {
		if got, err := sampleRatio(in); err != nil || got != want {
			t.Errorf("sampleRatio(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"abc", "-1", "1.5"} {
		if _, err := sampleRatio(in); err == nil {
			t.Errorf("sampleRatio(%q) should fail", in)
		}
	}
}

func hasAttr(attrs []attribute.KeyValue, want attribute.KeyValue) bool {
	for _, a := range attrs {
		if a == want {
			return true
		}
	}
	return false
}
