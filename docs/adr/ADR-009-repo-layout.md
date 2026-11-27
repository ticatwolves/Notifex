# ADR-009 — Repository layout: single root Go module

- **Status:** Accepted (2026-11-27)
- **Decider:** Shobhit
- **Resolves:** the open layout choice in docs/11 §Repo layout.

## Context
CLAUDE.md specifies a root layout (`cmd/`, `internal/`, `web/`, `insights/`, `tools/` at the top level). docs/11 proposed a nested monorepo (`services/core`, `services/insights`, `apps/web`) and asked for one decision, recorded in an ADR. The repo had just been reset to docs only, so either choice was free to make.

## Decision
- **One Go module at the repo root**, module path `github.com/sbshobhit/notifex`. Binaries live in `cmd/{api,dispatcher,scheduler,migrate,healthcheck}`, and libraries in `internal/` as laid out in CLAUDE.md.
- **Non-Go deployables are top-level siblings with their own toolchains:** `web/` (Next.js, ADR-003), `insights/` (Python/uv, ADR-002), `tools/` (Python engineering tooling). The Go SDK lives in `sdk/go` as its **own module**, so SDK consumers never pull server dependencies. The same goes for `terraform-provider-notifex/`.
- **Contracts** live in `api/` (`api/proto` with `buf.yaml` today, `api/openapi.yaml` from Phase 1). Generated Go code goes to `api/gen/` once codegen starts (Phase 4 for protobuf).
- **Deploy assets** go in `deploy/docker` (Dockerfiles, local service config) and `deploy/terraform`. `docker-compose.yml` sits at the root so `docker compose` works without flags.
- **One core image** carries every Go binary. ECS task definitions and compose choose the entrypoint (docs/11).

## Consequences
- `go test ./...` at the root covers all server code. Separate modules (`sdk/go`, the Terraform provider) run their own CI jobs.
- `internal/` keeps the server packages unimportable from the SDK and the provider. Anything they need goes through the generated API contract, never shared Go packages (charter §4).
- docs/11 §Repo layout is updated to match.

## Alternatives considered
- **`services/core` nested module (docs/11 draft).** This adds a directory level and a `go.work` or path prefix to every Go command, and gains nothing while there is only one Go service module. Rejected.
- **A module per binary.** That means version skew across binaries that share one schema and one deploy. Rejected.
