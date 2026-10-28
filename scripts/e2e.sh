#!/usr/bin/env bash
# End-to-end test: start the compose stack, generate traffic with the
# loadgen container, then check through the HTTP APIs that metrics, exemplars,
# logs and tail-sampled traces agree. Exits non-zero at the first failed check.
# Needs only docker (with compose v2), curl and jq: no Go on the host.
set -euo pipefail
cd "$(dirname "$0")/.."

step() { printf '\n== %s\n' "$*"; }
pass() { printf '   ok: %s\n' "$*"; }
fail() { printf '   FAIL: %s\n' "$*" >&2; exit 1; }

step "prerequisites"
for tool in docker curl jq; do
  command -v "$tool" >/dev/null || fail "$tool is required (install it, then run again)"
done
docker compose version >/dev/null 2>&1 || fail "docker compose v2 is required (docker compose version)"
docker info >/dev/null 2>&1 || fail "cannot reach the Docker daemon (is it running, are you in the docker group?)"
pass "docker, docker compose, curl, jq"

REQUESTS=${REQUESTS:-1000}
# The 80% and 70-90% thresholds below are only statistically safe from here.
((REQUESTS >= 500)) || fail "REQUESTS=$REQUESTS: use at least 500 (the sampling checks are statistical)"

# One e2e run at a time: concurrent runs would share this project.
# Its own project AND its own host ports: a dev stack started with `make up`
# (default ports, project "go-api-observability") keeps running untouched.
export COMPOSE_PROJECT_NAME=invoice-api-e2e
export APP_PORT=${E2E_APP_PORT:-18080}
export PROMETHEUS_PORT=${E2E_PROMETHEUS_PORT:-19090}
export JAEGER_PORT=${E2E_JAEGER_PORT:-26686}
export GRAFANA_PORT=${E2E_GRAFANA_PORT:-13000}
APP=http://127.0.0.1:$APP_PORT
PROM=http://127.0.0.1:$PROMETHEUS_PORT
JAEGER=http://127.0.0.1:$JAEGER_PORT
GRAFANA=http://127.0.0.1:$GRAFANA_PORT
export DEMO_FAULTS=1
# A throwaway password if none is set: no .env file is needed (or written).
if [[ -z "${GRAFANA_ADMIN_PASSWORD:-}" && -f .env ]]; then
  GRAFANA_ADMIN_PASSWORD=$(sed -n 's/^GRAFANA_ADMIN_PASSWORD=//p' .env)
fi
export GRAFANA_ADMIN_PASSWORD=${GRAFANA_ADMIN_PASSWORD:-$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')}

cleanup() {
  status=$?
  if [[ $status -ne 0 ]]; then
    docker compose logs --tail=40 >&2 || true
  fi
  step "docker compose down -v"
  docker compose --profile tools down -v --remove-orphans >/dev/null 2>&1 || true
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
int() { printf '%.0f' "${1:-0}"; }

step "docker compose up"
docker compose down -v --remove-orphans >/dev/null 2>&1 || true # leftovers of an aborted run
docker compose up -d --build --quiet-pull --wait --wait-timeout 180 || fail "stack did not become healthy"

step "wait for services"
retry 120 curl -fsS "$APP/healthz" || fail "app not ready"; pass "app"
retry 120 curl -fsS "$PROM/-/ready" || fail "prometheus not ready"; pass "prometheus"
retry 120 curl -fsS "$JAEGER/api/services" || fail "jaeger not ready"; pass "jaeger"
retry 120 curl -fsS "$GRAFANA/api/health" || fail "grafana not ready"; pass "grafana"
retry 120 has_value 'up{job="invoice-api"} == 1' || fail "prometheus does not scrape the app"; pass "app scraped"
retry 120 has_value 'up{job="otel-collector"} == 1' || fail "prometheus does not scrape the collector"; pass "collector scraped"
curl -fsS "$PROM/api/v1/rules" | jq -e '[.data.groups[].rules[].name] | index("InvoiceAPIErrorBudgetFastBurn") and index("InvoiceAPIErrorBudgetSlowBurn")' >/dev/null \
  || fail "SLO alert rules not loaded"
pass "SLO alert rules loaded"

step "load: $REQUESTS requests (5% errors, 5% slow)"
out=$(docker compose --profile tools run --rm -T --build --quiet-pull loadgen -n "$REQUESTS") \
  || { echo "$out"; fail "loadgen failed"; }
echo "$out"
sent_slow=$(sed -n 's/.*slow=\([0-9]*\).*/\1/p' <<<"$out")
[[ -n "$sent_slow" ]] || fail "cannot read the slow count from loadgen"

step "metrics: every request counted, route pattern labels only"
counted_q='sum(http_requests_total{job="invoice-api"})' # /healthz is never counted
counted_is_all() { [[ "$(int "$(promq "$counted_q")")" == "$REQUESTS" ]]; }
retry 60 counted_is_all || fail "Prometheus counted $(promq "$counted_q") requests, want $REQUESTS"
pass "http_requests_total = $REQUESTS (health probes not counted)"
routes=$(curl -fsS --get "$PROM/api/v1/label/route/values" --data-urlencode 'match[]=http_requests_total' | jq -r '.data[]')
pass "route values: $(tr '\n' ' ' <<<"$routes")"
while read -r route; do
  case "$route" in
    /invoices | "/invoices/{id}" | unmatched) ;; # /healthz must not appear
    *) fail "unexpected route label '$route' (raw path or ID?)" ;;
  esac
done <<<"$routes"
pass "only route patterns in the route label"

step "traces kept by tail sampling (decision_wait is 5s)"
jaeger_count() {
  curl -fsS --get "$JAEGER/api/traces" --data-urlencode service=invoice-api \
    --data-urlencode lookback=1h --data-urlencode limit=5000 "$@" | jq '.data | length'
}
errors=$(int "$(promq 'sum(http_requests_total{job="invoice-api",code=~"5.."})')")
((errors > 0)) || fail "no 5xx counted by Prometheus"
enough_errors() { (($(jaeger_count --data-urlencode 'tags={"error":"true"}') * 10 >= errors * 8)); }
retry 60 enough_errors || fail "error traces in Jaeger: $(jaeger_count --data-urlencode 'tags={"error":"true"}'), want >= 80% of $errors"
pass "error traces in Jaeger: $(jaeger_count --data-urlencode 'tags={"error":"true"}') for $errors 5xx"
enough_slow() { (($(jaeger_count --data-urlencode minDuration=500ms) * 10 >= sent_slow * 8)); }
retry 60 enough_slow || fail "slow traces in Jaeger: $(jaeger_count --data-urlencode minDuration=500ms), want >= 80% of $sent_slow"
pass "slow traces (>= 500ms) in Jaeger: $(jaeger_count --data-urlencode minDuration=500ms) for $sent_slow sent"

step "collector dropped most of the normal traces"
m='otelcol_processor_tail_sampling_global_count_traces_sampled'
all_q="sum({__name__=~\"${m}(_total)?\"})"
decided_is_all() { [[ "$(int "$(promq "$all_q")")" == "$REQUESTS" ]]; }
retry 60 decided_is_all || fail "collector decided on $(promq "$all_q") traces, want $REQUESTS"
dropped=$(int "$(promq "sum({__name__=~\"${m}(_total)?\",sampled=\"false\"})")")
kept=$(int "$(promq "sum({__name__=~\"${m}(_total)?\",sampled=\"true\"})")")
pass "decided=$REQUESTS kept=$kept dropped=$dropped"
# Expected: errors + slow (~10%) kept, 10% of the other ~90% kept => ~81% dropped.
awk -v d="$dropped" -v k="$kept" 'BEGIN { r = d / (d + k); printf "   dropped share: %.0f%%\n", r * 100; exit !(r >= 0.70 && r <= 0.90) }' \
  || fail "dropped share outside 70-90%"

step "exemplars: every one leads to a stored trace and a log line"
exemplar_ids() {
  curl -fsS --get "$PROM/api/v1/query_exemplars" \
    --data-urlencode 'query=http_request_duration_seconds_bucket{job="invoice-api"}' \
    --data-urlencode "start=$(($(date +%s) - 900))" --data-urlencode "end=$(date +%s)" |
    jq -r '[.data[].exemplars[].labels.trace_id // empty] | unique | .[]'
}
all_exemplars_in_jaeger() {
  local id n=0
  for id in $(exemplar_ids); do
    curl -fsS "$JAEGER/api/traces/$id" >/dev/null 2>&1 || return 1
    n=$((n + 1))
  done
  ((n > 0))
}
retry 60 all_exemplars_in_jaeger || fail "an exemplar trace_id is missing from Jaeger (or there is no exemplar)"
ids=$(exemplar_ids)
pass "$(wc -l <<<"$ids") exemplar trace_id(s), all found in Jaeger"
first=$(head -n1 <<<"$ids")
app_logs=$(docker compose logs --no-color app) # not piped into grep -q: SIGPIPE + pipefail
grep -qF "\"trace_id\":\"$first\"" <<<"$app_logs" || fail "exemplar trace_id $first not found in the app logs"
pass "exemplar trace_id $first found in the app logs"

step "grafana"
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

step "the public port does not serve /metrics"
code=$(curl -s -o /dev/null -w '%{http_code}' "$APP/metrics")
[[ "$code" == 404 ]] || fail "GET $APP/metrics returned $code, want 404"
pass "GET /metrics on the public port -> 404"

step "all checks passed"
