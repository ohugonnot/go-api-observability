# Go API observability: slog, runtime/metrics, Prometheus and OpenTelemetry

[Version française](README.fr.md)

A small invoice API in plain `net/http`, instrumented so that the three signals
point at each other:

- **logs** (`log/slog`, JSON): every request log line carries `trace_id` and `span_id`;
- **metrics** (Prometheus, RED method + Go runtime): slow and failed requests carry
  their `trace_id` as an **exemplar**, so a point on a Grafana latency graph opens the trace;
- **traces** (OpenTelemetry): server span from `otelhttp`, child span in the
  repository, the incoming W3C `traceparent` kept as a link (public endpoint), head sampling in the app and
  **tail sampling** in the Collector (errors and slow traces kept, ~10% of the rest).

Companion code for the article *"Observabilité d'une API Go : slog, Prometheus et
OpenTelemetry reliés par un trace_id"* by Odilon Hugonnot ([web-developpeur.com](https://web-developpeur.com)).

It is a teaching example, not a production template.

## Prerequisites

- **The stack** (`make up`, `make demo`, `make e2e`): Docker Engine with Compose v2
  (`docker compose version`); `curl` and `jq` are only needed by `make e2e`. Go is not needed: the API and the
  load generator are built in containers.
- **Local development** (`make run`, `make test`, `make lint`, `make vuln`): Go 1.26
  (the `toolchain` line of `go.mod` asks for go1.26.10).
- `make`, or run the commands of the `Makefile` by hand.

## Layout

```
main.go          config, servers (public :8080, internal metrics :9091), timeouts, SIGTERM shutdown
server.go        routes, input validation, middleware order
store.go         in-memory repository, child span, Invoice.LogValue (no PII in logs)
logging.go       slog handler adding trace_id/span_id, redaction safety net
metrics.go       RED metrics by route pattern, exemplars, panic recovery, Go runtime collector
tracing.go       TracerProvider, ParentBased(TraceIDRatioBased), span limits, OTLP exporter
demo.go          ?delay= and ?fail= (only with DEMO_FAULTS=1)
cmd/loadgen      traffic generator: normal, slow and failing requests
*_test.go        httptest, tracetest, prometheus/testutil, log/trace correlation
docker-compose.yml, Dockerfile, otel-collector/, prometheus/, grafana/   the local lab
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
make lint         # go vet, gofmt -l, staticcheck
make vuln         # govulncheck
make rules        # promtool check rules / test rules / check config (Docker)
```

### The full stack

```sh
cp .env.example .env    # then set GRAFANA_ADMIN_PASSWORD (see below)
make up                 # start the stack
make demo               # loadgen container: 1000 requests, 5% errors, 5% slow
make down               # stop it and delete its volumes (works without .env)
make e2e                # its own project and ports: up, demo, checks, down -v
```

A password: `openssl rand -base64 18`, or without openssl
`head -c 18 /dev/urandom | base64`. With an empty password, `docker compose`
refuses to start: that is on purpose.

Without `make`:

```sh
docker compose up -d --build --wait
docker compose --profile tools run --rm --build loadgen              # = make demo
docker compose --profile tools run --rm loadgen -n 3000 -errors 0.1  # other mixes
GRAFANA_ADMIN_PASSWORD=unused docker compose --profile tools down -v # = make down
./scripts/e2e.sh                                                     # = make e2e
```

Host ports, all bound to 127.0.0.1:

| Service    | Dev stack (`make up`), variable | `make e2e` default, override |
|------------|----------------------------------|------------------------------|
| API        | 8080, `APP_PORT`                 | 18080, `E2E_APP_PORT`        |
| Grafana    | 3000, `GRAFANA_PORT`             | 13000, `E2E_GRAFANA_PORT`    |
| Jaeger UI  | 16686, `JAEGER_PORT`             | 26686, `E2E_JAEGER_PORT`     |
| Prometheus | 9090, `PROMETHEUS_PORT`          | 19090, `E2E_PROMETHEUS_PORT` |

The app's metrics port (9091) and the Collector are never published. The e2e
test uses its own compose project and ports, so it can run while a dev stack is
up; run only one e2e at a time.

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

## The 2-minute tour

1. `make up`, then `make demo`, then wait about 15 seconds (the Collector
   decides after 5 s, Prometheus scrapes every 5 s).
2. Open Grafana (http://127.0.0.1:3000), dashboard **Invoice API - RED**.
3. In the **Duration** panel, the diamonds are exemplars: hover one and click
   its `trace_id` link. The trace opens in Jaeger: server span, `store.Get`
   child, `demo.delay` for a slow one.
4. Copy that `trace_id` and find the matching log lines:
   `docker compose logs app | grep <trace_id>`.

Few diamonds? Prometheus keeps about one exemplar per histogram bucket per
scrape, and only errors and slow requests carry one. Run `make demo` two or
three more times.

## What each piece shows

**Route pattern labels.** Metrics are labelled with `r.Pattern`
(`/invoices/{id}`), never `r.URL.Path` (`/invoices/42`): one series per route
instead of one per invoice. Unrouted requests get `route="unmatched"` and
unknown methods `method="OTHER"`, so clients cannot create series at will.

**Middleware order.** `otelhttp -> instrument -> mux`. The mux writes
`r.Pattern` on the `*http.Request` it receives; `instrument` must pass that same
pointer (no `r.WithContext`) to read it afterwards. Copying the request breaks
labels, span names and exemplars at once: six tests fail.

**Panics.** `instrument` recovers a panicking handler, logs it (type and stack)
and counts it as `code="500"`, so a crash cannot hide from the error rate. If
nothing was written yet, the client gets a 500; if part of the response is
already out, the connection is aborted (`http.ErrAbortHandler`) so the client
sees a failure instead of a truncated 200. A handler that panics with
`http.ErrAbortHandler` itself is a deliberate abort: re-panicked, not counted.

**Separate metrics port.** `/metrics` listens on `127.0.0.1:9091` by default
(`:9091` inside compose, where the port is not published). It is not part of the
API, it reveals internals (Go version, memory, routes), and scraping must not go
through the public port's auth, rate limiting or load balancer.

**Logs.** `traceHandler` wraps the JSON handler and adds `trace_id`/`span_id`
from the context (use `InfoContext` and friends). 5xx access logs are at
`ERROR` level. Like any attribute added by a handler, `trace_id` lands inside
the current group of a logger built with `WithGroup`; the request logger here
is never grouped, so it stays at the top level where log tools look for it.
Errors of the OpenTelemetry SDK itself (a failed export...) go to the same JSON
log at `ERROR` level (`otel.SetErrorHandler`).

**Health probes are invisible.** `GET /healthz` is answered before any
instrumentation: no span, no metric, no access log. Counted, the compose
healthcheck alone (every 5 s) would add free successes to the error ratio's
denominator and hide real errors, plus 17,000 log lines a day.

**Public endpoint vs. internal propagation.** On this public port an incoming
`traceparent` is untrusted: with `otelhttp.WithPublicEndpointFn`, every request
starts a new trace, sampled by our own policy, and only *links* to the
caller's context. Without it, a client could pick our `trace_id`, or send the
"not sampled" flag (`-00`) to make even its 500s disappear from traces and
exemplars. Between trusted internal services you want the opposite: plain
propagation, one trace across services. The trade-off: a trace started by a
trusted front end is split in two here, joined by the link.

**Personal data.** The defence is not logging it: `Invoice.LogValue` only logs
id, amount and currency, the bad-payload log does not echo the body, invalid
IDs are rejected before reaching logs or spans. A `ReplaceAttr` safety net lower-cases
each attribute key and strips `_`, `-` and `.`, then masks it when it equals,
or ends with, one of `email`, `password`, `passwd`, `token`, `secret`,
`secretkey`, `privatekey`, `accesskey`, `cookie`, `authorization`, `apikey`:
`user_email`, `accessToken`, `X-Api-Key`, `userpassword`, `db.password` are
masked, `token_count`, `max_tokens` or `email_verified` are not. Plurals
(`emails`, `cookies`) and other names (`credentials`, `session_id`) are not
covered. Its
limits: it only sees attribute keys, so a secret inside a map, in a struct
without `LogValue` or in an error message still goes through.

**Bounded input.** IDs must match `^[0-9]{1,18}$`, `customer` and `email` are
capped at 256 bytes, the currency is three ASCII capital letters, the body is
capped at 1 MiB (413 above), data after the JSON object is rejected (400), span
attribute values are truncated to 256 bytes, and both servers have read, write,
idle and header timeouts plus a 16 KiB header limit.

**Runtime metrics.** `collectors.NewGoCollector` with
`WithGoCollectorRuntimeMetrics(MetricsGC, MetricsMemory, MetricsScheduler)`
exposes `runtime/metrics` series (GC, heap, scheduler latencies).

**Sampling.** The app samples at the head with
`ParentBased(TraceIDRatioBased(TRACE_SAMPLE_RATIO))` (default 1). On this public
endpoint every request is a root, so the ratio decides; `ParentBased` matters
for child spans and for internal services that do trust their callers. In compose the ratio is 1
and the Collector decides at the tail, once the whole trace is known
(`decision_wait: 5s` for the demo): `status_code: ERROR` and `latency > 500ms`
are always kept, `probabilistic: 10` keeps a share of the rest. Do not combine a
head ratio below 1 with tail sampling if you want every error: the head drops
traces before the Collector knows they failed.

**Exemplars.** The duration histogram uses `ObserveWithExemplar` with the
`trace_id`, but **only for requests the Collector always keeps**: a 5xx, or a
request of at least 500 ms (`slowThreshold`, the same value as the Collector's
latency policy). A fast 200 is kept only 10% of the time, so its exemplar would
usually lead to a missing trace. `/metrics` serves OpenMetrics (required for
exemplars) and Prometheus runs with `--enable-feature=exemplar-storage`. The
Grafana Prometheus datasource maps `trace_id` to the Jaeger datasource.

**Error budget alerts.** `prometheus/rules/slo.yml`: SLO 99.9% over 30 days,
multi-window multi-burn-rate. Page at 14.4x (2% of the budget in 1h, checked on
1h and 5m), ticket at 6x (5% in 6h, on 6h and 30m). Only 5xx count: a 404 is
the client's error. `slo_test.yml` holds eight `promtool test rules` cases:
sustained burn (pending, then firing), fresh incident, finished spike with
steady 404s, very recent errors only, slow burn (pending, then firing),
finished slow burn, errors within budget, no error at all (ratio 0, not
"no data": the rules use `(errors or total * 0) / total`).

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
- **Jaeger 2.20.0, not 2.22.** The
  [v2.21.0 release notes](https://github.com/jaegertracing/jaeger/releases/tag/v2.21.0)
  list, under breaking changes, "remove v1 http endpoints the ui no longer calls
  ([#9260](https://github.com/jaegertracing/jaeger/pull/9260))". Grafana 13.2's
  Jaeger datasource still calls them. Reproduced on 2026-10-28 with
  jaegertracing/jaeger:2.22.0 and grafana/grafana:13.2.5: Jaeger's
  `GET /api/services` returned 404, and Grafana's
  `GET /api/datasources/uid/jaeger/health` returned
  `{"message":"request failed: 404 Not Found","status":"ERROR"}`. Moving to
  Tempo is the long-term fix.
- **Hardening, where many tutorials do otherwise:** no container runs as root,
  `cap_drop: ALL`, `no-new-privileges`, read-only root filesystems, memory and
  process limits (with `GOMEMLIMIT` at ~90% of the app's), healthchecks (the
  app probes itself with `-healthcheck`: its distroless image has no shell), the
  Grafana password comes from `.env` (never `admin/admin` in the compose file),
  admin UIs bind to `127.0.0.1`, Prometheus runs without
  `--web.enable-lifecycle`, the Go images are distroless `nonroot`.

## Versions

Go 1.26 (toolchain go1.26.10), go.opentelemetry.io/otel v1.47.0, otelhttp v0.72.0,
prometheus/client_golang v1.25.0, golang.org/x/net v0.62.0. Images:
otel/opentelemetry-collector-contrib:0.164.0, jaegertracing/jaeger:2.20.0,
prom/prometheus:v3.15.1, grafana/grafana:13.2.5, golang:1.26.10-alpine3.24,
gcr.io/distroless/static-debian13:nonroot (pinned by digest).

## Verification

Run by the author on 2026-10-28 (WSL2, Docker 29.1.3, Compose v2.40.3).

| What | How, and result |
|------|-----------------|
| formatting, vet, static analysis | `gofmt -l .`, `go vet ./...`, `staticcheck ./...` (v0.8.2): clean |
| unit tests, race detector | `go test -race ./...` in `golang:1.26.10-bookworm`: green |
| known vulnerabilities | `govulncheck ./...` (v1.9.0): none |
| alert rules | `promtool check rules`, `promtool test rules` (8 cases), `promtool check config`, in `prom/prometheus:v3.15.1` |
| load generator verdict | `go test ./cmd/loadgen`: small n, no failing request, missing or extra 5xx, NaN flags |
| Collector config | `otelcol-contrib validate` in the 0.164.0 image |
| whole stack | `scripts/e2e.sh` (the `make e2e` target) green twice in a row from a fresh clone, with a PATH without Go, while a dev stack (`docker compose up -d`, default ports) was running from another clone: 1000 requests counted, route labels limited to patterns, every 5xx and every slow request found in Jaeger once the Collector has decided on all 1000 traces (58/58 and 64/64 on the last run), 70-90% of traces dropped (observed 78-83%), every exemplar found in Jaeger and in the app logs, alert rules loaded, Grafana datasources healthy, `/metrics` 404 on the public port |
| the tests catch regressions | 54 Go mutations and 18 rule mutations, each one turned a test red (a local mutation script, not published): for example `r.URL.Path` as label -> `TestRequestCounterUsesRoutePattern`, no `recover` -> `TestPanicIsCountedAs500`, no abort after a partial write -> `TestPanicAfterWriteAbortsConnection`, no public endpoint -> `TestUnsampledTraceparentCannotHideA500`, health probes counted -> `TestHealthzIsNotCounted`, `validID` up to 40 digits -> `TestIDLengthBoundary`, exemplar on every request -> `TestExemplarsOnlyForKeptTraces`, `for: 2m` -> `40m` -> the pending/firing case of `slo_test.yml`. Removing the Collector's `errors` policy makes the e2e fail on the error-traces check |
| Jaeger 2.22 incompatibility | reproduced, see Design choices |

Not verified: behaviour under real load or with several Collector replicas, the
exemplar-to-trace click in
the Grafana UI itself (checked through the Grafana and Prometheus APIs), arm64
hosts, the `make` targets on the author's machine (`make` is not installed
there: the same commands were run directly, and `make -n` / `make lint test`
inside the Go image).

## Limits

- Teaching example: in-memory store (unbounded: every POST stays in memory
  until restart), no auth, no TLS between components,
  in-memory Jaeger, demo-sized intervals (5s scrape, 5s `decision_wait`).
- `DEMO_FAULTS` must be `0` outside the demo (`DEMO_FAULTS=0 make up`).
- A log `trace_id` of a fast, successful request may point to a trace the
  Collector dropped (about 90% of normal traffic).
- Tail sampling needs all spans of a trace on the same Collector: with several
  replicas, put a `loadbalancing` exporter in front.

## License

MIT, Copyright (c) 2026 Odilon Hugonnot.
