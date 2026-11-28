# Notifex

Multi-tenant notifications as a service. Tenants define workflows with per-channel templates (email, SMS, push, Slack, Outlook/Teams, WhatsApp, webhook). Their backends trigger a workflow by REST, gRPC or SDK, and Notifex renders, dispatches, retries and records the outcome of every notification.

Start with [CLAUDE.md](CLAUDE.md) and [docs/](docs/). Decisions are in [docs/adr/](docs/adr/).

## Quickstart (local)

Requires Go (version in `go.mod`), Docker, and GNU make.

```bash
make dev        # postgres, elasticmq, migrate (one-shot), api, dispatcher, scheduler
make seed       # local tenant + app + env; prints a fresh API key and a curl command
curl -s -H "Authorization: Bearer <key>" localhost:8080/v1/whoami
curl localhost:9100/readyz   # ops port: readiness detail + /metrics
make down
```

## What's built

- **Phase 0:** scaffold, CI, store with tenant scoping, migrations, envelope encryption, partition helpers, API-key auth (`/v1/whoami`).
- **Phase 1:** console API (signup, login, sessions, OAuth, tenants, members, invitations, RBAC, applications, environments, API keys, audit log) and the operator realm (`cmd/operator`, TOTP).
- **Phase 2:** workflows, channel actions and templates with a sandboxed Handlebars-subset renderer and preview (ADR-012); brand profiles and sending policies; channel credentials, platform and BYO, with write-only encrypted secrets (ADR-013).

The contract is [api/openapi.yaml](api/openapi.yaml): operations marked `implemented` are served and contract-tested; `draft` ones are designed for later phases.

## Schema changes

```bash
# edit ent/schema/*.go (and ent/schema/constraints.go for foreign keys)
make generate                     # regenerate ent code
make migration name=add_widgets   # write migrations/<ts>_add_widgets.up.sql (needs Docker)
make migrate                      # apply to the local compose database
```
Review generated SQL like code. See ADR-010.

## Gates (same targets run in CI)

```bash
make lint       # go vet + golangci-lint
make test       # go test -race ./...
make vuln       # govulncheck
make buf-lint   # protobuf contract lint
make migrate-check  # ent/schema and migrations/ in sync
make migrate-lint   # squawk: unsafe migration operations
make image      # linux/arm64 core image
```

## Ports

| Service | Port | Purpose |
|---|---|---|
| api | 8080 | public REST |
| api | 9090 | public gRPC |
| every service | 9100 (host 9100/9101/9102) | ops: `/metrics`, `/healthz`, `/readyz`; never exposed via the ALB |
| elasticmq | 9324 / 9325 | SQS API / stats UI |
| mailpit | 8025 / 1025 | email UI / SMTP (verification and reset emails land here) |
| postgres | 54320 (host) → 5432 | host port avoids a native Postgres; override with `POSTGRES_HOST_PORT` |
| jaeger (`--profile obs`) | 16686 / 4317 | tracing UI / OTLP |
