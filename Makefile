# Notifex developer entrypoints. CI calls these same targets.
SHELL := /bin/bash
.DEFAULT_GOAL := help

# Pinned tool versions (CI uses the same pins).
GOLANGCI_LINT_VERSION := v2.15.0
GOVULNCHECK_VERSION   := v1.10.0
BUF_VERSION           := v1.75.0
SQUAWK_VERSION        := 2.74.0
VACUUM_VERSION        := v0.43.0
LOCAL_DATABASE_URL    ?= postgres://notifex:notifex@localhost:54320/notifex?sslmode=disable
# Pepper and local master key default to public dev values when NOTIFEX_ENV=local.
LOCAL_ENV := NOTIFEX_ENV=local LOG_FORMAT=text DATABASE_URL=$(LOCAL_DATABASE_URL)

BIN     := $(CURDIR)/bin
VERSION ?= $(shell git rev-parse --short=12 HEAD 2>/dev/null || echo dev)
IMAGE   ?= notifex-core
SERVICES := api dispatcher scheduler migrate healthcheck seed

GOLANGCI_LINT := $(BIN)/golangci-lint-$(GOLANGCI_LINT_VERSION)
GOVULNCHECK   := $(BIN)/govulncheck-$(GOVULNCHECK_VERSION)
BUF           := $(BIN)/buf-$(BUF_VERSION)
VACUUM        := $(BIN)/vacuum-$(VACUUM_VERSION)

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
	  CGO_ENABLED=0 go build -trimpath -ldflags="-X github.com/ticatwolves/notifex/internal/app.Version=$(VERSION)" \
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
test: ## Unit + integration tests with the race detector (integration tests need Docker)
	go test -race -count=1 -coverprofile=coverage.out ./...
	@grep -v -E '/ent/[a-z_]+/|/ent/[a-z_]+\.go:|/internal/auth/oauthtest/|/internal/store/storetest/' coverage.out > coverage.handwritten.out
	@printf 'coverage (excluding generated ent code and test helpers): '; go tool cover -func=coverage.handwritten.out | tail -1 | awk '{print $$NF}'

.PHONY: test-short
test-short: ## Fast tests only (skips container-backed integration tests)
	go test -race -short -count=1 ./...

.PHONY: vuln
vuln: $(GOVULNCHECK) ## govulncheck
	$(GOVULNCHECK) ./...

.PHONY: buf-lint
buf-lint: $(BUF) ## Lint protobuf contracts
	cd api/proto && $(BUF) lint

.PHONY: api-lint
api-lint: $(VACUUM) ## Lint the OpenAPI spec (vacuum, Spectral-compatible rules)
	$(VACUUM) lint --ruleset api/vacuum-ruleset.yaml --details --errors api/openapi.yaml

.PHONY: check
check: lint test vuln buf-lint api-lint migrate-check ## Everything CI runs on Go code

## ---- codegen / db ----
.PHONY: generate
generate: ## Run code generators (ent; protobuf from Phase 4)
	go generate ./...

.PHONY: migration
migration: ## Generate a migration from ent/schema changes: make migration name=add_widgets
	@test -n "$(name)" || { echo "usage: make migration name=<snake_case>"; exit 2; }
	go run -mod=mod ./ent/migrate/main.go -name $(name)

.PHONY: migrate-check
migrate-check: ## Fail if ent/schema changed without a migration (needs Docker)
	go run -mod=mod ./ent/migrate/main.go -check

.PHONY: migrate-lint
migrate-lint: ## Lint migrations for unsafe operations (squawk; needs Node)
	npx -y squawk-cli@$(SQUAWK_VERSION) migrations/*.up.sql  # settings in .squawk.toml

.PHONY: migrate
migrate: ## Apply migrations to the local compose database
	$(LOCAL_ENV) go run ./cmd/migrate up

.PHONY: seed
seed: ## Seed a local tenant/app/env and print a fresh API key (stack must be up)
	@$(LOCAL_ENV) go run ./cmd/seed

## ---- tools ----
$(GOLANGCI_LINT):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	mv $(BIN)/golangci-lint $@

$(GOVULNCHECK):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) go install golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VERSION)
	mv $(BIN)/govulncheck $@

$(VACUUM):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) go install github.com/daveshanley/vacuum@$(VACUUM_VERSION)
	mv $(BIN)/vacuum $@

$(BUF):
	@mkdir -p $(BIN)
	GOBIN=$(BIN) go install github.com/bufbuild/buf/cmd/buf@$(BUF_VERSION)
	mv $(BIN)/buf $@

.PHONY: clean
clean: ## Remove build output
	rm -rf $(addprefix $(BIN)/,$(SERVICES)) coverage.out coverage.handwritten.out
