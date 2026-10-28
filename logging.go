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

// sensitiveWords is a safety net: an attribute is masked when its key (any
// case) is one of these words or ends with "_word", "-word" or ".word":
// "user_email", "X-Api-Key", "db.password" are masked, "token_count" or
// "email_verified" are not.
// It only sees attribute keys: a secret inside a map, a struct without
// LogValue, or an error message goes through. The real defence is not
// logging such values, and LogValue on business types (see Invoice).
var sensitiveWords = []string{
	"email", "password", "passwd", "token", "secret", "cookie", "set-cookie",
	"authorization", "api_key", "apikey", "api-key",
}

func redact(_ []string, a slog.Attr) slog.Attr {
	key := strings.ToLower(a.Key)
	for _, word := range sensitiveWords {
		if key == word || strings.HasSuffix(key, "_"+word) ||
			strings.HasSuffix(key, "-"+word) || strings.HasSuffix(key, "."+word) {
			return slog.String(a.Key, "[REDACTED]")
		}
	}
	return a
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(traceHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: redact})})
}
