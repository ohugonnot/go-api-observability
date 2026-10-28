#!/usr/bin/env bash
# End-to-end test: start the compose stack, generate traffic, then check
# through the HTTP APIs that metrics, exemplars and sampled traces arrived.
# Exits non-zero at the first failed check. Needs docker, go, curl and jq.
set -euo pipefail
cd "$(dirname "$0")/.."

APP=http://127.0.0.1:8080
PROM=http://127.0.0.1:9090
JAEGER=http://127.0.0.1:16686
GRAFANA=http://127.0.0.1:3000
REQUESTS=${REQUESTS:-1000}

# A throwaway password if none is set: no .env file is needed (or written).
if [[ -z "${GRAFANA_ADMIN_PASSWORD:-}" && -f .env ]]; then
  GRAFANA_ADMIN_PASSWORD=$(sed -n 's/^GRAFANA_ADMIN_PASSWORD=//p' .env)
fi
export GRAFANA_ADMIN_PASSWORD=${GRAFANA_ADMIN_PASSWORD:-$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')}

step() { printf '\n== %s\n' "$*"; }
pass() { printf '   ok: %s\n' "$*"; }
fail() { printf '   FAIL: %s\n' "$*" >&2; exit 1; }

cleanup() {
  status=$?
  if [[ $status -ne 0 ]]; then
    docker compose logs --tail=40 >&2 || true
  fi
  step "docker compose down -v"
  docker compose down -v --remove-orphans >/dev/null 2>&1 || true
  exit $status
}
trap cleanup EXIT

# retry SECONDS CMD...: run CMD every 2s until it succeeds or time runs out.
retry() {
  local deadline=$((SECONDS + $1)); shift
  until "$@" >/dev/null 2>&1; do
    ((SECONDS < deadline)) || return 1
    sleep 2
  done
}

# promq QUERY: instant query, prints the first value (empty if no result).
promq() {
  curl -fsS --get "$PROM/api/v1/query" --data-urlencode "query=$1" | jq -r '.data.result[0].value[1] // empty'
}
has_value() { [[ -n "$(promq "$1")" ]]; }

step "docker compose up"
docker compose up -d --build --quiet-pull

step "wait for services"
retry 120 curl -fsS "$APP/healthz" || fail "app not ready"; pass "app"
retry 120 curl -fsS "$PROM/-/ready" || fail "prometheus not ready"; pass "prometheus"
retry 120 curl -fsS "$JAEGER/api/services" || fail "jaeger not ready"; pass "jaeger"
retry 120 curl -fsS "$GRAFANA/api/health" || fail "grafana not ready"; pass "grafana"
retry 120 has_value 'up{job="invoice-api"} == 1' || fail "prometheus does not scrape the app"; pass "app scraped"
retry 120 has_value 'up{job="otel-collector"} == 1' || fail "prometheus does not scrape the collector"; pass "collector scraped"

step "load: $REQUESTS requests (5% errors, 5% slow)"
go run ./cmd/loadgen -url "$APP" -n "$REQUESTS"

step "metrics: route pattern labels, no raw IDs"
retry 60 has_value 'sum(http_requests_total{job="invoice-api",route="/invoices/{id}"})' \
  || fail "no http_requests_total{route=\"/invoices/{id}\"}"
pass "http_requests_total{route=\"/invoices/{id}\"} = $(promq 'sum(http_requests_total{job="invoice-api",route="/invoices/{id}"})')"
routes=$(curl -fsS --get "$PROM/api/v1/label/route/values" --data-urlencode 'match[]=http_requests_total' | jq -r '.data | join(" ")')
pass "route values: $routes"
if grep -Eq '/[0-9]+' <<<"$routes"; then fail "a route label contains a raw ID"; fi
pass "no raw ID in route labels"

step "exemplars"
exemplars() {
  curl -fsS --get "$PROM/api/v1/query_exemplars" \
    --data-urlencode 'query=http_request_duration_seconds_bucket{job="invoice-api"}' \
    --data-urlencode "start=$(($(date +%s) - 900))" --data-urlencode "end=$(date +%s)" |
    jq -r '[.data[].exemplars[].labels.trace_id] | map(select(. != null)) | .[0] // empty'
}
has_exemplar() { [[ -n "$(exemplars)" ]]; }
retry 60 has_exemplar || fail "no exemplar with a trace_id"
pass "exemplar trace_id=$(exemplars)"

step "traces kept by tail sampling (decision_wait is 5s)"
jaeger_count() {
  curl -fsS --get "$JAEGER/api/traces" --data-urlencode service=invoice-api \
    --data-urlencode lookback=1h --data-urlencode limit=1000 "$@" | jq '.data | length'
}
has_traces() { (($(jaeger_count "$@") > 0)); }
retry 60 has_traces --data-urlencode 'tags={"error":"true"}' || fail "no error trace in Jaeger"
pass "error traces in Jaeger: $(jaeger_count --data-urlencode 'tags={"error":"true"}')"
retry 30 has_traces --data-urlencode minDuration=500ms || fail "no slow trace in Jaeger"
pass "slow traces (>= 500ms) in Jaeger: $(jaeger_count --data-urlencode minDuration=500ms)"

step "an exemplar leads to a trace stored in Jaeger"
exemplar_in_jaeger() {
  local id
  for id in $(curl -fsS --get "$PROM/api/v1/query_exemplars" \
    --data-urlencode 'query=http_request_duration_seconds_bucket{job="invoice-api"}' \
    --data-urlencode "start=$(($(date +%s) - 900))" --data-urlencode "end=$(date +%s)" |
    jq -r '.data[].exemplars[].labels.trace_id // empty'); do
    if curl -fsS "$JAEGER/api/traces/$id" >/dev/null 2>&1; then
      echo "$id"
      return 0
    fi
  done
  return 1
}
retry 30 exemplar_in_jaeger || fail "no exemplar trace_id found in Jaeger"
pass "exemplar trace_id=$(exemplar_in_jaeger) is in Jaeger"

step "collector dropped part of the normal traces"
m='otelcol_processor_tail_sampling_global_count_traces_sampled'
dropped_q="sum({__name__=~\"${m}(_total)?\",sampled=\"false\"})"
kept_q="sum({__name__=~\"${m}(_total)?\",sampled=\"true\"})"
all_q="sum({__name__=~\"${m}(_total)?\"})"
retry 60 has_value "$all_q >= $REQUESTS" || fail "collector did not decide on all $REQUESTS traces"
has_value "$dropped_q > 0" || fail "collector reports no dropped trace"
dropped=$(promq "$dropped_q"); kept=$(promq "$kept_q")
pass "kept=$kept dropped=$dropped"
# Expected: 10% errors+slow kept, ~10% of the other 90% kept => ~80% dropped.
awk -v d="$dropped" -v k="${kept:-0}" 'BEGIN { r = d / (d + k); printf "   dropped share: %.0f%%\n", r * 100; exit !(r > 0.6 && r < 0.95) }' \
  || fail "dropped share outside 60-95%"

step "grafana datasources"
for uid in prometheus jaeger; do
  curl -fsS -u "admin:$GRAFANA_ADMIN_PASSWORD" "$GRAFANA/api/datasources/uid/$uid/health" | jq -e '.status == "OK"' >/dev/null \
    || fail "grafana datasource $uid is not healthy"
  pass "datasource $uid healthy"
done
curl -fsS -u "admin:$GRAFANA_ADMIN_PASSWORD" "$GRAFANA/api/datasources/uid/prometheus" |
  jq -e '.jsonData.exemplarTraceIdDestinations[0].datasourceUid == "jaeger"' >/dev/null \
  || fail "exemplar link from Prometheus to Jaeger not configured"
pass "exemplar trace_id links to the jaeger datasource"
curl -fsS -u "admin:$GRAFANA_ADMIN_PASSWORD" "$GRAFANA/api/dashboards/uid/invoice-api-red" | jq -e '.dashboard.title' >/dev/null \
  || fail "RED dashboard not provisioned"
pass "RED dashboard provisioned"

step "all checks passed"
