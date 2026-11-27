# Notifex developer entrypoints. CI calls these same targets.
SHELL := /bin/bash
.DEFAULT_GOAL := help

# Pinned tool versions (CI uses the same pins).
GOLANGCI_LINT_VERSION := v2.15.0
GOVULNCHECK_VERSION   := v1.10.0
BUF_VERSION           := v1.75.0

BIN     := $(CURDIR)/bin
VERSION ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo dev)
IMAGE   ?= notifex-core
SERVICES := api dispatcher scheduler migrate healthcheck

GOLANGCI_LINT := $(BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
GOVULNCHECK   := $(BIN)/govulncheck-$(GOVULNCHECK_VERSION)
BUF           := $(BIN)/buf-$(BUF_VERSION)

.PHONY: help
help: ## Show targets
	@grep -hE '^[a-zA-Z_-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  \033[36m%-14s\033[0m %s\n",$$1,$$2}'

## ---- local stack ----
.PHONY: dev
dev: ## Start the local stack (postgres, elasticmq, api, dispatcher, scheduler)
	VERSION=$(VERSION) docker compose up --build -d
	@echo "api http://localhost:8080  grpc localhost:9090  ops :9100/:9101/:9102  elasticmq :9325"

.PHONY: down
down: ## Stop the local stack (keeps volumes)
	docker compose down

.PHONY: logs
logs: ## Tail local stack logs
	docker compose logs -f api dispatcher scheduler

## ---- build ----
.PHONY: build
build: ## Build all binaries into ./bin
	@for svc in $(SERVICES); do \
	  CGO_ENABLED=0 go build -trimpath -ldflags="-X github.com/sbshobhit/notifex/internal/app.Version=$(VERSION)" \
	    -o $(BIN)/$$svc ./cmd/$$svc || exit 1; \
	done

.PHONY: image
image: ## Build the core image for linux/arm64
	docker buildx build --platform linux/arm64 --build-arg VERSION=$(VERSION) \
	  -f deploy/docker/core.Dockerfile -t $(IMAGE):$(VERSION) --load .

## ---- quality gates ----
.PHONY: fmt
fmt: $(GOLANGCI_LINT) ## Format code
	$(GOLANGCI_LINT) fmt

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) vet ## golangci-lint + go vet
	$(GOLANGCI_LINT) run

.PHONY: test
test: ## Unit + integration tests with the race detector
	go test -race -count=1 -coverprofile=coverage.out ./...

.PHONY: test-short
test-short: ## Fast tests only (skips container-backed integration tests)
	go test -race -short -count=1 ./...

.PHONY: vuln
vuln: $(GOVULNCHECK) ## govulncheck
	$(GOVULNCHECK) ./...

.PHONY: buf-lint
buf-lint: $(BUF) ## Lint protobuf contracts
	cd api/proto && $(BUF) lint

.PHONY: check
check: lint test vuln buf-lint ## Everything CI runs on Go code

## ---- codegen / db (filled in by later slices) ----
.PHONY: generate
generate: ## Run code generators (ent in slice 3, protobuf in Phase 4)
	go generate ./...

.PHONY: migrate
migrate: ## Apply migrations (slice 3)
	go run ./cmd/migrate up

## ---- tools ----
$(GOLANGCI_LINT):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(BIN)/golangci-lint $@

$(GOVULNCHECK):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	mv $(BIN)/govulncheck $@

$(BUF):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	mv $(BIN)/buf $@

.PHONY: clean
clean: ## Remove build output
	rm -rf $(BIN)/api $(BIN)/dispatcher $(BIN)/scheduler $(BIN)/migrate $(BIN)/healthcheck coverage.out
