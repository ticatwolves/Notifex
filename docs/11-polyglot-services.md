# 11 — Go, Python and Next.js: Boundaries, Contracts, and Build

Three languages is a deliberate choice. It only works if each has a clear job and the seams are contracts, not shared code.

## Ownership
| Runtime | Owns | Must not |
|---|---|---|
| **Go (core)** | Trigger intake (REST + gRPC), auth/RBAC, control-plane API, dispatcher, scheduler, channel adapters, policy engine, template rendering, **every write to core tables**, vendor callbacks | Host analytics/ML experiments or notebooks |
| **Python (insights)** | Batch and analytics: dashboard rollups, usage metering aggregation (billing export), failure-pattern/anomaly detection, tenant digests and exports, later AI-assisted features (template drafting, failure explanations); plus engineering tooling (contract fuzzing with schemathesis, pytest API/E2E harness, load-result analysis, seed data) | Sit on the trigger→dispatch path, write core tables directly, hold vendor credentials |
| **Next.js (web)** | Console UI, server-side rendering, session handling UX | Access the database, hold secrets, duplicate business logic |

Rule of thumb: **if an outage of it would stop notifications from being accepted or sent, it is Go.** Python failing must never affect delivery.

## Contracts between runtimes
- **OpenAPI 3.1** (REST) and **protobuf** (gRPC) in `api/` are the source of truth; generate Go server stubs, TypeScript client (for web), Python client (for insights/tools), and SDKs from them. `buf breaking` and OpenAPI diff run in CI.
- **Data access for Python:** a read-only Postgres role limited to documented views/tables on the read replica (primary at Stage A/B). Python writes only to schemas it owns (`analytics`, `billing_export`). Anything that changes core state goes through the Go API with a service credential.
- **Events:** if Python needs to react to events, consume from the outbox/queue topic once SQS exists; before then, poll rollup tables.
- **Versioning:** additive changes only inside `v1`; deprecation window for removals.

## Repo layout (monorepo, ADR-009)
```
notifex/
├── go.mod                  # single Go module: github.com/sbshobhit/notifex
├── cmd/{api,dispatcher,scheduler,migrate,healthcheck}/
├── internal/               # Go core libraries (layout in CLAUDE.md)
├── api/                    # proto/ (+ buf.yaml), openapi.yaml, gen/ (generated)
├── web/                    # Next.js app (App Router)
├── insights/               # Python (uv/pyproject): src/notifex_insights, tests/, jobs/
├── sdk/{go,node,python}/   # sdk/go is its own Go module
├── terraform-provider-notifex/   # its own Go module
├── tools/                  # python: contract tests, e2e (pytest/playwright), load analysis, seed
├── k6/
├── deploy/{docker,terraform}/
└── docs/
```
Decided in ADR-009: root layout per CLAUDE.md, not a nested `services/core` module.

## Docker images (ARM64, small, non-root)
**Go core** (one image, several entrypoints). The implemented version is `deploy/docker/core.Dockerfile`: base images pinned by digest, plus a `healthcheck` binary for container health checks, since distroless has no curl.
```dockerfile
FROM --platform=$BUILDPLATFORM golang:1.24 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/dispatcher ./cmd/dispatcher \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/scheduler ./cmd/scheduler \
 && CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/migrate ./cmd/migrate
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ /app/
USER nonroot
ENTRYPOINT ["/app/api"]   # ECS task definitions override `command` per service
```
**Python insights:**
```dockerfile
FROM python:3.13-slim AS build
COPY --from=ghcr.io/astral-sh/uv:latest /uv /bin/uv
WORKDIR /app
COPY pyproject.toml uv.lock ./
RUN uv sync --frozen --no-dev
COPY src ./src
FROM python:3.13-slim
RUN useradd -r -u 10001 app
WORKDIR /app
COPY --from=build /app /app
ENV PATH="/app/.venv/bin:$PATH"
USER app
ENTRYPOINT ["python", "-m", "notifex_insights"]   # job name passed as command/args
```
**Next.js** (`next.config`: `output: 'standalone'`):
```dockerfile
FROM node:22-slim AS deps
WORKDIR /app
COPY package*.json ./
RUN npm ci
FROM node:22-slim AS build
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN npm run build
FROM node:22-slim
WORKDIR /app
ENV NODE_ENV=production PORT=3000 HOSTNAME=0.0.0.0
COPY --from=build /app/.next/standalone ./
COPY --from=build /app/.next/static ./.next/static
COPY --from=build /app/public ./public
USER node
CMD ["node", "server.js"]
```
(These are starting points; Claude Code must build, scan and run them, and pin base image digests.)

## Service topology and runtime config
| Service | Image | CPU/mem start | Scaling | Notes |
|---|---|---|---|---|
| `api` | core | 0.5 vCPU / 1 GB (A) → 1 / 2 (B) | CPU + RPS | REST (`:8080`) + gRPC (`:9090`) |
| `dispatcher` | core | 0.25–0.5 vCPU / 0.5–1 GB | queue depth, oldest-job age | Spot-friendly, graceful drain |
| `scheduler` | core | 0.25 / 0.5 | fixed 1 (leader lease) | delayed jobs, retries, retention |
| `migrate` | core | one-off task | n/a | runs before deploys |
| `pgbouncer` | upstream image | 0.25 / 0.5 | fixed | transaction mode |
| `web` | web | 0.25–0.5 / 0.5–1 | CPU | behind CloudFront |
| `insights-*` | insights | 0.25–1 / 0.5–2 | scheduled | EventBridge Scheduler → RunTask, Spot |

Config is 12-factor env vars + SSM/Secrets Manager references in task definitions. Same binaries run locally and in AWS.

## Frontend ↔ backend rules (Next.js)
- The browser talks only to the Next.js app and, for the public data plane docs/examples, to the documented API.
- Server components and route handlers call the Go API with the user's session; the Go API remains the only authority for authorization (UI hiding is cosmetic).
- Generate the typed client from OpenAPI; never hand-write API types.
- Sessions: HttpOnly, Secure, SameSite cookies, CSRF protection for mutations; short-lived access + refresh handled by the Go auth service.
- Next.js 16/React 19: async `params`, `cookies()`, `headers()`; no `fetch` of internal URLs from client components; no secrets in `NEXT_PUBLIC_*`.
- Template preview renders in a sandboxed iframe (`sandbox`, no scripts) with a strict CSP.

## Quality gates per runtime (in CI)
- Go: `go vet`, `golangci-lint`, `go test -race`, `govulncheck`, gosec.
- Python: `ruff`, `mypy`/`pyright`, `pytest`, `pip-audit`/`osv-scanner`, bandit; type hints required.
- Web: `eslint`, `tsc --noEmit`, Vitest, Playwright, `npm audit`, bundle-size budget, Lighthouse CI.
- Cross-cutting: contract tests, Trivy image scans, gitleaks, tfsec/checkov, SBOM + image signing.

## Prompts
- "Add the Python `insights` service skeleton (uv, ruff, mypy, pytest) with one job, `usage_rollup`, that reads usage from the read-only role and writes to `analytics.usage_daily`. Include an ECS scheduled-task module entry and tests with a real Postgres container. It must not write core tables."
- "Generate TypeScript and Python clients from api/openapi.yaml in CI and fail the build if generated code is out of date."
- "Containerize core, insights and web for linux/arm64 with the Dockerfiles in docs/11; add Trivy scan and size reports to CI."
