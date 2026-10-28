// Command invoice-api is a small invoice API showing logs (slog), metrics
// (Prometheus) and traces (OpenTelemetry) wired together.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := newLogger(os.Stdout)
	slog.SetDefault(log)

	ratio, err := sampleRatio(os.Getenv("TRACE_SAMPLE_RATIO"))
	if err != nil {
		return err
	}
	tp, err := newTracerProvider(ctx, ratio)
	if err != nil {
		return err
	}
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	reg := newRegistry()
	public := &http.Server{
		Addr:              envOr("ADDR", ":8080"),
		Handler:           newHandler(log, tp, reg, os.Getenv("DEMO_FAULTS") == "1"),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// /metrics lives on a separate, internal listener: it is not part of the
	// public API, it reveals internals, and scraping must not depend on the
	// public port's routing, auth or rate limits.
	internal := &http.Server{
		Addr:              envOr("METRICS_ADDR", "127.0.0.1:9091"),
		Handler:           metricsHandler(reg),
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 2)
	for _, srv := range []*http.Server{public, internal} {
		go func() {
			log.Info("listening", "addr", srv.Addr)
			if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		log.Info("shutting down")
	}

	// Drain HTTP first (in-flight requests still create spans), then flush spans.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return errors.Join(
		public.Shutdown(shutdownCtx),
		internal.Shutdown(shutdownCtx),
		tp.Shutdown(shutdownCtx),
	)
}
