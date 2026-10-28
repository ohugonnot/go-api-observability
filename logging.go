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
// Like any attribute added in Handle, they land inside the current group if
// the logger was built with WithGroup: this service never groups its
// request logger, so they stay at the top level where log tools look.
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

// sensitiveWords is a safety net. The key is lower-cased and stripped of
// "_", "-" and "."; it is masked when it equals one of these words, or ends
// with one of at least 5 letters: "accessToken", "user_email", "X-Api-Key",
// "db.password" are masked, "token_count", "max_tokens" or "email_verified"
// are not.
// It only sees attribute keys: a secret inside a map, a struct without
// LogValue, or an error message goes through. The real defence is not
// logging such values, and LogValue on business types (see Invoice).
var sensitiveWords = []string{
	"email", "password", "passwd", "token", "secret", "secretkey", "privatekey",
	"accesskey", "cookie", "authorization", "apikey",
}

var keySeparators = strings.NewReplacer("_", "", "-", "", ".", "")

func redact(_ []string, a slog.Attr) slog.Attr {
	key := keySeparators.Replace(strings.ToLower(a.Key))
	for _, word := range sensitiveWords {
		if key == word || (len(word) >= 5 && strings.HasSuffix(key, word)) {
			return slog.String(a.Key, "[REDACTED]")
		}
	}
	return a
}

func newLogger(w io.Writer) *slog.Logger {
	return slog.New(traceHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{ReplaceAttr: redact})})
}
