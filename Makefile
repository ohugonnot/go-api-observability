PROMETHEUS_IMAGE := prom/prometheus:v3.15.1
PROMTOOL := docker run --rm --entrypoint promtool -v "$(CURDIR)/prometheus:/etc/prometheus:ro" -w /etc/prometheus/rules $(PROMETHEUS_IMAGE)
STATICCHECK := honnef.co/go/tools/cmd/staticcheck@v0.8.2
GOVULNCHECK := golang.org/x/vuln/cmd/govulncheck@v1.9.0

.PHONY: run test lint vuln up down demo e2e rules

run: ## run the API locally (no Collector: spans are created, not exported)
	DEMO_FAULTS=1 go run .

test:
	go test ./...

lint:
	go vet ./...
	@out=$$(gofmt -l .); if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi
	go run $(STATICCHECK) ./...

vuln:
	go run $(GOVULNCHECK) ./...

up:
	docker compose up -d --build --wait

down:
	docker compose --profile tools down -v

demo: ## send mixed traffic to the running stack (no Go needed)
	docker compose --profile tools run --rm --build loadgen

e2e:
	./scripts/e2e.sh

rules: ## check and unit-test the alerting rules with promtool
	$(PROMTOOL) check rules slo.yml
	$(PROMTOOL) test rules slo_test.yml
	$(PROMTOOL) check config ../prometheus.yml
