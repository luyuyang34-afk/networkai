# Common tasks. `make help` lists them.
TEST_DATABASE_URL ?= postgres://networkai:networkai@localhost:5432/networkai?sslmode=disable
TEST_ES_URL       ?= http://localhost:9200
TEST_REDIS_URL    ?= redis://localhost:6379/0
TEST_KAFKA_BROKERS ?= localhost:19092

.PHONY: help up down logs lint test test-integration seed demo-outage reindex \
	synth train load-synth rebuild loadtest web-test

help: ## Show this help
	@grep -E '^[a-z-]+:.*##' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  %-18s %s\n",$$1,$$2}'

up: ## Build and start the full stack
	docker compose up --build -d
	@echo "API: http://localhost:8080  (next: make seed)"

down: ## Stop the stack (add -v manually to also delete data volumes)
	docker compose down

logs: ## Tail api and indexer logs
	docker compose logs -f api indexer

lint: ## gofmt + go vet
	@test -z "$$(gofmt -l .)" || (echo "gofmt needed:"; gofmt -l .; exit 1)
	go vet ./...

check-alerts: ## Validate Prometheus rules and run the SLO burn-rate alert tests
	docker run --rm -v $(CURDIR)/deploy/prometheus:/p -w /p --entrypoint promtool prom/prometheus:v3.5.0 check rules alerts.yml slo.yml
	docker run --rm -v $(CURDIR)/deploy/prometheus:/p -w /p --entrypoint promtool prom/prometheus:v3.5.0 test rules slo_test.yml

test: ## Unit tests (integration tests skip without databases)
	go test -race ./...

test-integration: ## All tests against PostgreSQL + Elasticsearch from docker compose
	docker compose up -d --wait postgres elasticsearch redis redpanda
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' TEST_ES_URL='$(TEST_ES_URL)' TEST_REDIS_URL='$(TEST_REDIS_URL)' \
		TEST_KAFKA_BROKERS='$(TEST_KAFKA_BROKERS)' \
		go test -race -count=1 ./...

seed: ## Create demo users and posts
	./scripts/seed.sh

demo-outage: ## Live demo: stop Elasticsearch, keep posting, recover with no data loss
	./scripts/demo-es-outage.sh

reindex: ## Re-apply all posts to the search index in place
	docker compose run --rm indexer reindex

rebuild: ## Blue/green: build a new index (with embeddings), swap the alias
	docker compose run --rm indexer reindex -fresh

synth: ## Generate the synthetic dataset (5k users, 100k posts, 1.7M impressions)
	python3 tools/synth/generate.py --out data/synth

train: ## Train embeddings + ranker and regenerate docs/ranking-eval.md
	python3 tools/ranking/train_eval.py --data data/synth --models models --report docs/ranking-eval.md

load-synth: ## Load the synthetic dataset into PostgreSQL, then rebuild the index
	tools/synth/load.sh data/synth
	$(MAKE) rebuild

API_IP = $$(docker inspect networkai-api-1 --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}')
loadtest: ## k6 mixed workload against the API container (RATE=500 DURATION=60s)
	k6 run -e API=http://$(API_IP):8080 -e RATE=$${RATE:-500} -e DURATION=$${DURATION:-60s} tools/loadtest/mixed.js

web-test: ## Frontend lint, typecheck, build and Playwright e2e (needs `make up seed`)
	cd web && npm ci && npm run lint && npm run typecheck && npm run build && npx playwright test
