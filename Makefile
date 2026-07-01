# Brainiac — developer tasks. See SYSTEM.md §3.
.PHONY: help fmt lint test build tidy up down

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo 0.0.0-dev)
LDFLAGS := -X github.com/programmism/brainiac/internal/core.Version=$(VERSION)

help: ## List targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-8s\033[0m %s\n", $$1, $$2}'

fmt: ## Format code
	gofmt -w .

lint: ## Run golangci-lint
	golangci-lint run

test: ## Run tests
	go test ./...

build: ## Build all binaries into bin/
	go build -ldflags "$(LDFLAGS)" -o bin/ ./cmd/...

tidy: ## Tidy go.mod/go.sum
	go mod tidy

up: ## Start the local stack (docker compose)
	docker compose up -d

down: ## Stop the local stack
	docker compose down
