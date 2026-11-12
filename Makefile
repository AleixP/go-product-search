ARGS ?=
DATA_URL ?= https://static.openfoodfacts.org/data/en.openfoodfacts.org.products.csv.gz
GOLANGCI_LINT_VERSION := v2.15.0

.DEFAULT_GOAL := help

.PHONY: help up down ingest data run test lint

help: ## List the available targets
	@grep -h -E '^[A-Za-z0-9_.-]+:.*## ' $(MAKEFILE_LIST) | awk -F ':.*## ' '{printf "%-8s %s\n", $$1, $$2}'

up: ## Start OpenSearch with docker compose and wait until healthy
	docker compose up -d --wait

down: ## Stop OpenSearch (keeps the data volume)
	docker compose down

ingest: ## Load the dataset into OpenSearch (pass flags with ARGS="...")
	go run ./cmd/ingest $(ARGS)

data: ## Download a local copy of the dataset to data/ (about 1.5 GB)
	mkdir -p data
	curl -fL --retry 3 -o data/products.csv.gz.part $(DATA_URL) || { rm -f data/products.csv.gz.part; exit 1; }
	mv data/products.csv.gz.part data/products.csv.gz

run: ## Start the search API (pass flags with ARGS="...")
	go run ./cmd/api $(ARGS)

test: ## Run the Go tests with the race detector (-short)
	go test -race -short ./...

# Requires golangci-lint v2.15.0 (GOLANGCI_LINT_VERSION) installed locally (no auto-install).
lint: ## Run golangci-lint (needs the pinned local version)
	golangci-lint run ./...
