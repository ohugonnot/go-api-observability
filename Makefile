PROMETHEUS_IMAGE := prom/prometheus:v3.15.1

.PHONY: run test lint up down demo e2e rules

run: ## run the API locally (no Collector: spans are created, not exported)
	DEMO_FAULTS=1 go run .

test:
	go test ./...

lint:
	go vet ./...
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

up:
	docker compose up -d --build

down:
	docker compose down -v

demo: ## send mixed traffic to a running stack
	go run ./cmd/loadgen -url http://127.0.0.1:8080 -n 1000

e2e:
	./scripts/e2e.sh

rules: ## check and unit-test the alerting rules with promtool
	docker run --rm --entrypoint promtool -v "$(CURDIR)/prometheus:/etc/prometheus:ro" -w /etc/prometheus/rules \
		$(PROMETHEUS_IMAGE) check rules slo.yml
	docker run --rm --entrypoint promtool -v "$(CURDIR)/prometheus:/etc/prometheus:ro" -w /etc/prometheus/rules \
		$(PROMETHEUS_IMAGE) test rules slo_test.yml
	docker run --rm --entrypoint promtool -v "$(CURDIR)/prometheus:/etc/prometheus:ro" \
		$(PROMETHEUS_IMAGE) check config /etc/prometheus/prometheus.yml
