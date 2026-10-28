package main

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

// traceHandler adds trace_id and span_id to every record logged with a
// context that carries a span (use the *Context logging methods).
// The trace_id comes from the incoming traceparent: clients choose it.
type traceHandler struct{ slog.Handler }

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", sc.TraceID().String()),
			slog.String("span_id", sc.SpanID().String()),
		)
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{h.Handler.WithGroup(name)}
}

// sensitiveKeyParts is a safety net: an attribute whose key contains one of
// these (any case) is masked, e.g. "user_email" or "X-Api-Key".
// It only sees attribute keys: a secret inside a map, a struct without
// LogValue, or an error message goes through. The real defence is not
// logging such values, and LogValue on business types (see Invoice).
var sensitiveKeyParts = []string{
	"email", "passw", "token", "secret", "cookie", "authorization", "api_key", "apikey", "api-key",
}

func redact(_ []string, a slog.Attr) slog.Attr {
	key := strings.ToLower(a.Key)
	for _, part := range sensitiveKeyParts {
		if strings.Contains(key, part) {
			return slog.String(a.Key, "[REDACTED]")
		}
	}
	return a
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(traceHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: redact})})
}
