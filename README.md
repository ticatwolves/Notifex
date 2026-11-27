# Notifex

Multi-tenant notifications as a service. Tenants define workflows with per-channel templates (email, SMS, push, Slack, Outlook/Teams, WhatsApp, webhook). Their backends trigger a workflow by REST, gRPC or SDK, and Notifex renders, dispatches, retries and records the outcome of every notification.

Start with [CLAUDE.md](CLAUDE.md) and [docs/](docs/). Decisions are in [docs/adr/](docs/adr/).

## Quickstart (local)

Requires Go (version in `go.mod`), Docker, and GNU make.

```bash
make dev        # postgres, elasticmq, api, dispatcher, scheduler
curl localhost:8080/healthz
curl localhost:9100/readyz   # ops port: readiness detail + /metrics
make down
```

## Gates (same targets run in CI)

```bash
make lint       # go vet + golangci-lint
make test       # go test -race ./...
make vuln       # govulncheck
make buf-lint   # protobuf contract lint
make image      # linux/arm64 core image
```

## Ports

| Service | Port | Purpose |
|---|---|---|
| api | 8080 | public REST |
| api | 9090 | public gRPC |
| every service | 9100 (host 9100/9101/9102) | ops: `/metrics`, `/healthz`, `/readyz`; never exposed via the ALB |
| elasticmq | 9324 / 9325 | SQS API / stats UI |
| postgres | 5432 | |
| jaeger (`--profile obs`) | 16686 / 4317 | tracing UI / OTLP |
