# Common tasks. `make help` lists them.
TEST_DATABASE_URL ?= postgres://networkai:networkai@localhost:5432/networkai?sslmode=disable
TEST_ES_URL       ?= http://localhost:9200

.PHONY: help up down logs lint test test-integration seed demo-outage reindex

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

test: ## Unit tests (integration tests skip without databases)
	go test -race ./...

test-integration: ## All tests against PostgreSQL + Elasticsearch from docker compose
	docker compose up -d --wait postgres elasticsearch
	TEST_DATABASE_URL='$(TEST_DATABASE_URL)' TEST_ES_URL='$(TEST_ES_URL)' go test -race -count=1 ./...

seed: ## Create demo users and posts
	./scripts/seed.sh

demo-outage: ## Live demo: stop Elasticsearch, keep posting, recover with no data loss
	./scripts/demo-es-outage.sh

reindex: ## Rebuild the search index from PostgreSQL
	docker compose run --rm indexer reindex
