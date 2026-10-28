# Go API observability: slog, runtime/metrics, Prometheus and OpenTelemetry

[Version française](README.fr.md)

A small invoice API in plain `net/http`, instrumented so that the three signals
point at each other:

- **logs** (`log/slog`, JSON): every request log line carries `trace_id` and `span_id`;
- **metrics** (Prometheus, RED method + Go runtime): latency observations carry the
  `trace_id` as an **exemplar**, so a point on a Grafana latency graph opens the trace;
- **traces** (OpenTelemetry): server span from `otelhttp`, child span in the
  repository, W3C `traceparent` propagation, head sampling in the app and
  **tail sampling** in the Collector (errors and slow traces kept, ~10% of the rest).

Companion code for the article *"Observabilité d'une API Go : slog, runtime/metrics,
Prometheus et OpenTelemetry"* by Odilon Hugonnot ([web-developpeur.com](https://web-developpeur.com)).

It is a teaching example, not a production template.

## Layout

```
main.go          servers (public :8080, internal metrics :9091), SIGTERM shutdown
server.go        routes, handlers, middleware order
store.go         in-memory repository, child span, Invoice.LogValue (no PII in logs)
logging.go       slog handler adding trace_id/span_id, redaction safety net
metrics.go       RED metrics labelled by route pattern, exemplars, Go runtime collector
tracing.go       TracerProvider, ParentBased(TraceIDRatioBased), OTLP exporter
demo.go          ?delay= and ?fail= (only with DEMO_FAULTS=1)
cmd/loadgen      traffic generator: normal, slow and failing requests
*_test.go        httptest, tracetest, prometheus/testutil, log/trace correlation
docker-compose.yml, otel-collector/, prometheus/, grafana/   the local lab
scripts/e2e.sh   end-to-end test of the whole stack
```

## Run

```sh
make run          # DEMO_FAULTS=1 go run .   (API on :8080, metrics on 127.0.0.1:9091)
curl localhost:8080/invoices/1
curl -X POST localhost:8080/invoices -d '{"customer":"Bob","email":"bob@example.com","amount_cents":500,"currency":"EUR"}'
curl 127.0.0.1:9091/metrics
```

Without `OTEL_EXPORTER_OTLP_ENDPOINT`, spans are created (logs still get a
`trace_id`) but not exported.

```sh
make test         # go test ./...
make lint         # go vet + gofmt -l
make rules        # promtool check rules / test rules / check config (Docker)
```

### The full stack

```sh
cp .env.example .env    # then set GRAFANA_ADMIN_PASSWORD
make up                 # docker compose up -d --build
make demo               # 1000 requests: 5% errors, 5% slow, the rest normal
make down               # docker compose down -v
make e2e                # up, demo, checks through the HTTP APIs, down -v
```

| URL                         | What                                        |
|-----------------------------|---------------------------------------------|
| http://127.0.0.1:8080       | the API                                     |
| http://127.0.0.1:3000       | Grafana (admin / your .env password), "Invoice API - RED" dashboard |
| http://127.0.0.1:16686      | Jaeger UI                                   |
| http://127.0.0.1:9090       | Prometheus (alerts under /alerts)           |

```
app ──OTLP/HTTP──> otel-collector (tail_sampling) ──OTLP/gRPC──> jaeger
 │ :9091/metrics            │ :8888 internal metrics
 └──────────── prometheus (exemplar storage, SLO rules) <───┘
                         grafana: Prometheus + Jaeger datasources, exemplar -> trace
```

## What each piece shows

**Route pattern labels.** Metrics are labelled with `r.Pattern`
(`/invoices/{id}`), never `r.URL.Path` (`/invoices/42`): one series per route
instead of one per invoice. Unrouted requests get `route="unmatched"` and
unknown methods `method="OTHER"`, so clients cannot create series at will.
`TestRequestCounterUsesRoutePattern` fails if you switch the label to
`r.URL.Path`.

**Middleware order.** `otelhttp -> instrument -> mux`. The mux writes
`r.Pattern` on the `*http.Request` it receives; `instrument` must pass that same
pointer (no `r.WithContext`) to read it afterwards. Copying the request breaks
labels, span names and exemplars at once; several tests catch it.

**Separate metrics port.** `/metrics` listens on `:9091`, which the compose
file does not publish. It is not part of the API, it reveals internals (Go
version, memory, routes), and scraping must not go through the public port's
auth, rate limiting or load balancer.

**Logs.** `traceHandler` wraps the JSON handler and adds `trace_id`/`span_id`
from the context (use `InfoContext` and friends). Personal data stays out:
`Invoice.LogValue` only logs id, amount and currency, the bad-payload log does
not echo the body, and a `ReplaceAttr` deny-list (`email`, `authorization`...)
masks mistakes.

**Runtime metrics.** `collectors.NewGoCollector` with
`WithGoCollectorRuntimeMetrics(MetricsGC, MetricsMemory, MetricsScheduler)`
exposes `runtime/metrics` series (GC, heap, scheduler latencies).

**Sampling.** The app samples at the head with
`ParentBased(TraceIDRatioBased(TRACE_SAMPLE_RATIO))` (default 1): a request
arriving with a sampled `traceparent` stays sampled. In compose the ratio is 1
and the Collector decides at the tail, once the whole trace is known
(`decision_wait: 5s` for the demo): `status_code: ERROR` and `latency > 500ms`
are always kept, `probabilistic: 10` keeps a share of the rest.

**Exemplars.** The duration histogram is observed with `ObserveWithExemplar`
and `trace_id`; `/metrics` serves OpenMetrics (required for exemplars) and
Prometheus runs with `--enable-feature=exemplar-storage`. The Grafana Prometheus
datasource maps `trace_id` to the Jaeger datasource.

**Error budget alerts.** `prometheus/rules/slo.yml`: SLO 99.9% over 30 days,
multi-window multi-burn-rate. Page at 14.4x (2% of the budget in 1h, checked on
1h and 5m), ticket at 6x (5% in 6h, on 6h and 30m). `slo_test.yml` is a
`promtool test rules` unit test.

### Logs to Loki (not included)

The stack has no log backend: logs go to stdout. To click from a log line to its
trace, ship the JSON lines to Loki (Grafana Alloy, or the Collector's
`filelog` receiver reading the container logs) and add a derived field to the
Loki datasource:

```yaml
jsonData:
  derivedFields:
    - name: trace_id
      matcherRegex: '"trace_id":"(\w+)"'
      datasourceUid: jaeger
      url: "$${__value.raw}"
```

It was left out to keep the lab small: it needs access to the Docker log files
or a logging driver plugin, i.e. more privileges than the rest of the stack.

## Design choices

- **Separate images rather than `grafana/otel-lgtm`.** That image is great for a
  quick look, but tail sampling needs the *contrib* Collector
  (`tail_sampling` is not in the core distribution), and seeing each component's
  config is the point of the article.
- **Jaeger 2.20.0, not 2.22.** Jaeger 2.21 removed the v1 HTTP query endpoints
  (`/api/services`, `/api/traces`) that Grafana 13.2's Jaeger datasource calls:
  with 2.22.0 the datasource health check returned `404 Not Found`. Moving to
  Tempo is the long-term fix.
- **Hardening, where many tutorials do otherwise:** no container runs as root,
  `cap_drop: ALL` and `no-new-privileges`, the Grafana password comes from `.env`
  (never `admin/admin` in the compose file), admin UIs bind to `127.0.0.1`,
  Prometheus runs without `--web.enable-lifecycle`, the app image is distroless
  `nonroot`.

## Versions

Go 1.26 (toolchain go1.26.10), go.opentelemetry.io/otel v1.47.0, otelhttp v0.72.0,
prometheus/client_golang v1.25.0. Images: otel/opentelemetry-collector-contrib:0.164.0,
jaegertracing/jaeger:2.20.0, prom/prometheus:v3.15.1, grafana/grafana:13.2.5,
golang:1.26.10-alpine3.24, gcr.io/distroless/static-debian13:nonroot (pinned by digest).

## Verification

Run by the author on 2026-10-28 (WSL2, Docker 29.1.3, Compose v2.40.3), with the
versions listed above:

- `go vet`, `gofmt -l`, `go test ./...` and `go test -race ./...` green
  (the race run inside the `golang:1.26.10-bookworm` image);
- `promtool check rules`, `promtool test rules` and `promtool check config`
  through the `prom/prometheus:v3.15.1` image, and
  `otelcol-contrib validate` on the Collector config;
- `docker compose up` and `scripts/e2e.sh` (the `make e2e` target) green twice
  in a row, then `docker compose down -v`. Typical run: 1000 requests,
  ~50 errors and ~50 slow ones, the Collector kept ~180 traces and dropped
  ~82%, error and slow traces present in Jaeger, an exemplar `trace_id`
  resolved in Jaeger, both Grafana datasources healthy.

Mutation checks: labelling with `r.URL.Path` makes
`TestRequestCounterUsesRoutePattern` fail; dropping `trace_id` from the slog
handler makes `TestLogsCarryTraceIDAndNoPersonalData` fail; passing
`r.WithContext(...)` to the mux makes five tests fail; raising the page
threshold from 14.4 to 25 makes `promtool test rules` fail.

Not verified: behaviour under real load, the GitHub Actions workflow (not run
on GitHub yet), the exemplar-to-trace click in the Grafana UI (checked through
the APIs only), arm64 hosts, and the `make` targets on the author's machine
(`make` is not installed there: the same commands were run directly, and
`make -n` / `make lint test` inside the Go image).

## Limits

- Teaching example: in-memory store, no auth, no TLS between components,
  in-memory Jaeger, demo-sized intervals (5s scrape, 5s `decision_wait`).
- `DEMO_FAULTS=1` must never be set in a real service.
- An exemplar or a log `trace_id` can point to a trace the Collector dropped
  (about 90% of normal traffic). Errors and slow requests, the ones you click
  on, are kept.
- Tail sampling needs all spans of a trace on the same Collector: with several
  replicas, put a `loadbalancing` exporter in front.

## License

MIT, Copyright (c) 2026 Odilon Hugonnot.
