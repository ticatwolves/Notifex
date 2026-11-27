# CLAUDE.md — Notifex (Notification as a Service)

You are helping build **Notifex**, a multi-tenant SaaS platform for notifications. Read this file fully at the start of every session, then read the docs it points to before touching code.

## What Notifex is (30-second version)

Tenants sign up, create **applications**, define **workflows** (each workflow = a notification), and attach **channel templates** (email, SMS, push, Slack, Outlook/Teams, WhatsApp, webhook). Their backends then trigger a workflow through our **SDK, REST API, or gRPC endpoint** with a payload and recipient details. Notifex renders, dispatches, retries, and records the outcome per channel. Tenants never run notification infrastructure; we scale it. Every workflow run and every single notification is observable. Each tenant has its own brand values (logo, colors, sender identity, footer) and policies (quiet hours, rate limits, allowed channels, retention).

Think "New Relic for notifications": a standalone, singleton, horizontally scalable service. **There is no subscriber/user model.** Callers pass recipient details inline with each trigger.

@docs/ENGINEERING_CHARTER.md

## Read these docs first (in order)

1. `docs/01-product-context.md` — personas, scope, non-goals, glossary
2. `docs/02-architecture.md` — system design, dispatch pipeline (SQS + outbox), scaling, tenancy
3. `docs/03-domain-and-api.md` — entities, API/gRPC/SDK contracts, statuses, error codes
4. `docs/04-roadmap-and-prompts.md` — phased build plan with ready-to-use prompts
5. `docs/05-test-strategy.md`, `docs/06-security-testing.md`, `docs/07-load-testing.md` — quality, security, performance plans
6. `docs/08-launch-plan.md`, `docs/09-competitive-landscape.md` — go-to-market and market context
7. `docs/10-aws-architecture.md`, `docs/11-polyglot-services.md` — AWS deployment, costs, Go/Python/Next.js boundaries
8. `docs/12-access-protocols-and-integrations.md` — protocols, MCP server for AI tools, Terraform actions, credentials and guardrails
9. `docs/adr/` — decisions. **ADR-004 to ADR-008 override anything older that disagrees with them.**
   - ADR-001 AWS ECS Fargate · ADR-002 Go core / Python insights · ADR-003 Next.js separate deployable
   - ADR-004 SQS dispatch queue + outbox · ADR-005 platform-managed + BYO credentials (platform SMS via SNS) · ADR-006 retention + encryption · ADR-007 pricing per delivered notification · ADR-008 region and AZs · ADR-009 repo layout

If code contradicts these docs, stop and flag it. Do not silently pick one.

## Locked decisions (do not re-litigate without asking)

- **Backend:** Go is the core platform (Gin HTTP, gRPC `google.golang.org/grpc`, ent ORM, PostgreSQL). **Python** is used only for insights/batch jobs and engineering tooling (read-only DB access, never on the trigger→dispatch path). See `docs/11-polyglot-services.md` and ADR-002.
- **Isolation boundary:** a `store.Store` interface. Handlers and services never import ent directly.
- **Channels:** a channel registry. Each channel implements a common `Channel` interface. Real vendors sit behind a `VendorClient` interface with a `MockVendorClient` for dev and tests.
- **Dispatch (ADR-004):** **Amazon SQS standard queues**, one per channel plus a DLQ each, with `MessageGroupId = tenant_id` (SQS fair queues). PostgreSQL is the **system of record**. The accept transaction writes `run` + `notifications` + **`dispatch_outbox`** rows; a relay publishes to SQS. **At-least-once** delivery; the real claim is a conditional status update on `notifications` (with a lease), so duplicate messages are harmless. Idempotency keys prevent duplicate triggers from callers.
- **Queue messages carry IDs only:** never payload, recipients or rendered content.
- **Delays:** SQS delay ≤ 15 min only. Anything longer (scheduled triggers, quiet hours, long backoff) goes through the Postgres `scheduled_dispatch` table and the scheduler.
- **Queue seam:** the dispatcher sits behind a `JobQueue` interface. No SQS types outside `internal/dispatch/sqsqueue`. **Kafka is a future implementation, not built now**; keep the envelope versioned and keyed by `tenant_id`.
- **Config snapshot at trigger acceptance:** when a trigger is accepted, snapshot the workflow, templates, brand, policy and the **resolved credential reference** into the run. Later edits never affect in-flight runs.
- **No `workflow_versions` / `template_versions` tables.** Templates are workflow-scoped.
- **Workflow actions are independent and parallel.** One channel failing (or hitting a limit) never blocks another.
- **Vendor credentials (ADR-005):** per channel, **platform-managed** (Notifex's accounts) or **BYO** (tenant's). Resolution: env BYO → app BYO → tenant BYO → platform. Platform SMS = **Amazon SNS**, no DLT in v1, **per-tenant daily SMS limit** enforced atomically at accept time, plus a platform-wide cap below the AWS spend quota. Push and WhatsApp are BYO-only at launch.
- **Retention and encryption (ADR-006):** payload/PII retention per tenant **1–7 days (max 7)**; notification metadata **90 days**; billing records per statute. Sensitive columns are envelope-encrypted with a **per-tenant data key** (KMS); tenant deletion = crypto-shred. Scheduled triggers max **7 days** ahead.
- **Billing (ADR-007):** charge **per delivered notification**; platform SMS bills on **`sent`**. `usage_records` is unique per `notification_id`, written in the same tx as the billable status transition.
- **Tenancy:** every tenant-owned row carries `tenant_id`. Every query is tenant-scoped through the store layer. Cross-tenant access is a critical bug.
- **Console roles:** `owner`, `admin`, `developer`, `viewer` (tenant-level), plus a separate **platform operator** role for running Notifex itself. See the permissions matrix in `docs/03-domain-and-api.md`.
- **Auth:** email/password registration plus OAuth (Google, GitHub, Microsoft) for the console. API keys (hashed at rest) for programmatic triggers.
- **Frontend:** Next.js (App Router, server components), Tailwind v4, dark ops-console aesthetic. Mind Next.js 16 / React 19 async request APIs.
- **Access surfaces:** one `trigger.Service`; transports (REST, gRPC, Connect, MCP, Terraform actions, CLI, inbound webhooks) stay thin and carry `source` + `actor` on every run. **All configuration lives in the portal**; MCP and Terraform only trigger/observe. AI/Terraform access is default-deny (workflows must be exposed in the portal) and every guardrail is enforced server-side. See `docs/12-access-protocols-and-integrations.md`.
- **Hosting (ADR-001, ADR-008):** AWS **ap-south-1** only. ECS Fargate on ARM64 (Spot for dispatcher), RDS PostgreSQL, SQS, SNS (SMS), SES, ALB + CloudFront + WAF, Terraform, GitHub Actions with OIDC. Network and compute are **multi-AZ from day one**; RDS is single-AZ until before production launch, then `multi_az = true`.
- **IaC:** Terraform for infra. A Terraform provider (`terraform-plugin-framework`) lets tenants trigger workflows and (optionally) manage config as code.

## Engineering rules

**Always**
- Make the smallest change that fully solves the task. Match existing patterns before inventing new ones.
- Put business logic in `internal/service/*`, not handlers.
- Pass `context.Context` first; propagate cancellation and deadlines. Wrap errors with `%w`.
- Write table-driven tests. Every new store method gets a test against real Postgres (testcontainers or docker-compose), not just mocks.
- Add a tenant-isolation test for any new store method that reads or writes tenant data.
- Test queue code against **ElasticMQ** (SQS-compatible) in integration tests, including duplicate delivery and redelivery after lease expiry.
- Emit structured logs (`slog`) with `tenant_id`, `run_id`, `notification_id`, `channel`, and `trace_id`. Propagate `traceparent` through the SQS envelope.
- Emit OpenTelemetry traces and Prometheus metrics for anything on the trigger → dispatch path.
- Write an audit event for every control-plane mutation (create/update/delete of apps, workflows, templates, keys, members, policies, credentials, plan limits).
- Run `go vet ./...`, `golangci-lint run`, and `go test ./... -race` before declaring a task done.

**Never**
- Never log secrets, API keys, OAuth tokens, vendor credentials, or full message bodies/recipient PII. Mask recipients (`a***@d***.com`, `+91******1234`).
- Never put payload, recipients or rendered content in queue messages.
- Never store vendor credentials or payload/recipient data in plaintext. Use envelope encryption via `KeyProvider` (KMS in AWS, local key in dev).
- Never call a real vendor (or real SNS/SES) in tests. Use `MockVendorClient`.
- Never add a subscriber/contact table. Recipients are per-trigger input.
- Never introduce versions tables, a new queue technology (including Kafka), or a new web framework without asking.
- Never edit generated code (ent, protobuf) by hand. Change the schema or `.proto` and regenerate.
- Never skip migrations review: all schema changes ship as reviewed migration files.
- Never commit `.env*` files; use `.env.example` with placeholders.

## Repo layout (target, ADR-009)

```
notifex/
├── CLAUDE.md
├── docs/  (+ docs/adr/)
├── api/proto/                # .proto files (gRPC), buf config
├── cmd/
│   ├── api/                  # REST + gRPC server (accept path, outbox fast-path publish)
│   ├── dispatcher/           # SQS consumers, one pool per channel
│   ├── mcp/                  # MCP server (stateless Streamable HTTP + stdio via CLI)
│   ├── migrate/              # applies versioned SQL migrations (one-off ECS task before deploy)
│   ├── healthcheck/          # tiny HTTP probe for container health checks (distroless image)
│   └── scheduler/            # outbox relay, scheduled_dispatch, purge/retention, partitions, rollups (leader-elected)
├── internal/
│   ├── auth/                 # sessions, OAuth, API keys, RBAC
│   ├── tenant/               # tenancy, brand, policy, plan limits
│   ├── workflow/             # workflows, actions, template rendering
│   ├── trigger/              # trigger intake, idempotency, snapshot, credential resolution
│   ├── dispatch/             # JobQueue, envelope, worker loop, retry/backoff
│   │   ├── outbox/           # outbox writer + relay
│   │   ├── sqsqueue/         # SQS JobQueue impl (only place that imports the SQS SDK)
│   │   └── inmem/            # in-memory JobQueue for unit tests
│   ├── channel/              # registry + email/sms/push/slack/outlook/whatsapp/webhook
│   │   └── vendor/           # VendorClient impls (ses, sns, twilio, …) + MockVendorClient
│   ├── credential/           # channel credentials, resolution, platform vs BYO
│   ├── crypto/               # KeyProvider, per-tenant DEKs, field encryption
│   ├── policy/               # quiet hours, rate limits, channel allowlists, daily SMS limits
│   ├── billing/              # usage records, rollups, billing-provider export
│   ├── observability/        # metrics, tracing, run/notification timeline
│   ├── store/                # Store interface + ent implementation
│   ├── audit/
│   └── transport/{http,grpc,connect,mcp,hooks}/
├── ent/schema/
├── sdk/{go,node,python}/
├── terraform-provider-notifex/
├── cli/
├── web/                      # Next.js console (separate deployable)
├── insights/                 # Python: batch/analytics jobs (see docs/11)
├── tools/                    # Python: contract tests, e2e harness, load analysis, seed data
├── deploy/{docker,terraform}/
└── Makefile
```

## Common commands

```
make dev          # docker-compose: postgres, elasticmq, api, dispatcher, scheduler, web, mock vendors
make generate     # ent + protobuf + openapi
make migrate      # apply migrations
make test         # go test ./... -race
make lint
make load-test    # k6 trigger-path benchmark
```
(If a target doesn't exist yet, create it as part of the task.)

## How to work with me (Shobhit)

- I am a senior backend engineer (Go, AWS, Terraform, distributed systems). Skip basics; explain trade-offs and edge cases, not syntax.
- **Plan first** for anything touching the dispatch pipeline, tenancy, credentials, encryption, billing, or the schema: give a short plan, call out risks, wait for my OK.
- For small, well-scoped tasks, just do them and summarize.
- When something is ambiguous, ask one focused question instead of guessing.
- Keep summaries tight: what changed, what you tested, what's left, any risk.
- Flag anything that would block production-readiness (security, data loss, tenant leakage, unbounded queues, missing idempotency, double billing).

## Definition of done

A task is done only when: code compiles, tests pass with `-race`, lint is clean, tenant isolation is tested, audit events and metrics/traces are in place where relevant, docs/OpenAPI/proto are updated, and the summary lists open risks.
