// Command invoice-api is a small invoice API showing logs (slog), metrics
// (Prometheus) and traces (OpenTelemetry) wired together.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

type config struct {
	Addr        string  // public API
	MetricsAddr string  // internal /metrics, loopback unless overridden
	SampleRatio float64 // head sampling ratio for root spans
	DemoFaults  bool    // ?delay= and ?fail=, demo only
}

func loadConfig(getenv func(string) string) (config, error) {
	ratio, err := sampleRatio(getenv("TRACE_SAMPLE_RATIO"))
	if err != nil {
		return config{}, err
	}
	or := func(key, fallback string) string {
		if v := getenv(key); v != "" {
			return v
		}
		return fallback
	}
	return config{
		Addr:        or("ADDR", ":8080"),
		MetricsAddr: or("METRICS_ADDR", "127.0.0.1:9091"),
		SampleRatio: ratio,
		DemoFaults:  getenv("DEMO_FAULTS") == "1",
	}, nil
}

func main() {
	// -healthcheck: probe /healthz of a running instance and exit 0 or 1.
	// The distroless image has no shell or curl for a compose healthcheck.
	check := flag.Bool("healthcheck", false, "probe the local /healthz and exit")
	flag.Parse()
	if *check {
		if err := healthcheck(envOr("ADDR", ":8080")); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy:", err)
			os.Exit(1)
		}
		return
	}
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

	cfg, err := loadConfig(os.Getenv)
	if err != nil {
		return err
	}
	tp, err := newTracerProvider(ctx, cfg.SampleRatio)
	if err != nil {
		return err
	}
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	reg := newRegistry()
	public := newServer(cfg.Addr, newHandler(log, tp, reg, cfg.DemoFaults))
	// /metrics lives on a separate, internal listener: it is not part of the
	// public API, it reveals internals, and scraping must not depend on the
	// public port's routing, auth or rate limits.
	internal := newServer(cfg.MetricsAddr, metricsHandler(reg))

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
	return shutdown(tp, public, internal)
}

// healthcheck GETs /healthz on the loopback side of a listen address like ":8080".
func healthcheck(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

// newServer sets the timeouts http.Server lacks by default (slow clients
// could otherwise hold connections forever).
func newServer(addr string, h http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second, // above maxDemoDelay
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
}

// shutdown drains HTTP first (in-flight requests still create spans), then
// flushes spans, each with its own budget: 8s + 5s stays below the 20s
// stop_grace_period of the compose file.
func shutdown(tp interface{ Shutdown(context.Context) error }, servers ...*http.Server) error {
	srvCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var errs []error
	for _, srv := range servers {
		errs = append(errs, srv.Shutdown(srvCtx))
	}

	tpCtx, cancelTP := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelTP()
	errs = append(errs, tp.Shutdown(tpCtx))
	return errors.Join(errs...)
}
