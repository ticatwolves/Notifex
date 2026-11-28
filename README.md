# Notifex

Multi-tenant notifications as a service. Tenants define workflows with per-channel templates (email, SMS, push, Slack, Outlook/Teams, WhatsApp, webhook). Their backends trigger a workflow by REST, gRPC or SDK, and Notifex renders, dispatches, retries and records the outcome of every notification.

Start with [CLAUDE.md](CLAUDE.md) and [docs/](docs/). Decisions are in [docs/adr/](docs/adr/).

## Quickstart (local)

Requires Go (version in `go.mod`), Docker, and GNU make.

```bash
make dev        # postgres, elasticmq, migrate (one-shot), api, dispatcher, scheduler
make seed       # local tenant + app + env + active "order_shipped" workflow; prints a fresh API key
curl -s -H "Authorization: Bearer <key>" localhost:8080/v1/whoami
curl -s -H "Authorization: Bearer <key>" -H 'Content-Type: application/json' localhost:8080/v1/trigger \
  -d '{"workflow":"order_shipped","recipients":[{"email":"a@example.test","phone":"+919876543210"}],"payload":{"order_id":"A1","tracking_url":"https://example.test/t"}}'
make load-smoke # k6: 50 triggers/s for 1 min, then checks every notification was sent and billed once
curl localhost:9100/readyz   # ops port: readiness detail + /metrics
make down
```

## What's built

- **Phase 0:** scaffold, CI, store with tenant scoping, migrations, envelope encryption, partition helpers, API-key auth (`/v1/whoami`).
- **Phase 1:** console API (signup, login, sessions, OAuth, tenants, members, invitations, RBAC, applications, environments, API keys, audit log) and the operator realm (`cmd/operator`, TOTP).
- **Phase 2:** workflows, channel actions and templates with a sandboxed Handlebars-subset renderer and preview (ADR-012); brand profiles and sending policies; channel credentials, platform and BYO, with write-only encrypted secrets (ADR-013).
- **Phase 3:** trigger intake (`POST /v1/trigger`, idempotency, config snapshots, encrypted payloads, atomic platform-SMS reservation, one-transaction accept with the outbox); SQS queues with the outbox relay and a leader-elected scheduler; the dispatcher (claim with lease, quiet hours and rate limits, retries with backoff, billing in the same transaction); channel adapters for SES, SNS, SendGrid, SMTP, Twilio, Slack and signed webhooks behind an SSRF-guarded dialer (ADR-014). Locally the dispatcher sends to the mock vendor (`DISPATCH_VENDORS=mock`).

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
