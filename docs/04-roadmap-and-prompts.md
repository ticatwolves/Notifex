# 04 — Roadmap and Ready-to-Use Prompts

> Updated 2026-11-27 for ADR-004 to ADR-008.

Work in phases. Each phase ends with a demo-able, tested slice. Start every phase in **plan mode**, review, then execute. Tell Claude Code: *"Read CLAUDE.md and docs/ (including docs/adr/) first."*

## Phase 0 — Foundations
- Repo scaffold, Makefile, docker-compose (**postgres, elasticmq**, mock vendors), CI (lint, test -race, buf, migrations check).
- ent schemas for core entities, migrations, `store.Store` interface + ent implementation, tenant-scoping helpers.
- `crypto.KeyProvider` (local key in dev, KMS interface for AWS) and per-tenant DEK lifecycle.
- Daily partitioning helpers for `notifications` / `notification_events`.
- Config, slog, OpenTelemetry, Prometheus wiring.

> **Prompt:** "Read CLAUDE.md, docs/ and docs/adr/. Plan Phase 0: scaffold the repo per the target layout, set up docker-compose with Postgres and ElasticMQ, CI, the ent schemas for Tenant, User, Membership, Application, Environment, ApiKey, and the KeyProvider interface with a local implementation. Show the plan, the store interface and the KeyProvider interface first. Don't write code until I approve."

## Phase 1 — Control plane core
- Registration, login, OAuth, sessions; tenants, memberships, RBAC middleware with the permissions matrix.
- Applications, environments, API keys (hash, prefix, revoke).
- Plans and limits (quotas, `platform_sms_daily_limit`, sandbox flag); operator realm basics.
- Audit events on every mutation.

> **Prompt:** "Implement auth (password + Google/GitHub/Microsoft OAuth) and RBAC per docs/03. Add a table-driven permission test covering every role × capability. Every mutation writes an audit event."

## Phase 2 — Workflows, templates, credentials, rendering
- Workflow, Action, Template CRUD; payload schema validation; template renderer with brand injection; preview endpoint.
- Brand profile and policy CRUD (incl. `payload_retention_days` 1–7).
- **ChannelCredential** CRUD with platform/BYO modes and resolution order (ADR-005); BYO secrets encrypted via KeyProvider.
- **Plan approved 2026-11-27** (ADR-012, ADR-013). Slices: 1 contract + ADRs · 2 renderer · 3 brand + policy · 4 workflows, actions, templates, preview · 5 channel credentials · 6 sweep + docs. Deferred: action `conditions`, workflow exposure flags, real credential verification (Phase 3/4).

> **Prompt:** "Implement the sandboxed template renderer and the preview endpoint. Include brand variables, per-channel validation (SMS ≤ 3 segments, push size, Slack blocks), and fuzz tests for the renderer. Then plan the ChannelCredential model and resolution (env BYO → app BYO → tenant BYO → platform) per ADR-005; wait for approval before the schema."

## Phase 3 — Trigger and dispatch (the critical path)
- Trigger intake (REST first): auth, idempotency, quota, credential resolution, **atomic platform-SMS daily reservation**, snapshot, encrypted payload, **single-transaction write of run + notifications + outbox / scheduled_dispatch**, fast-path publish.
- `JobQueue` with `sqsqueue` and `inmem` impls; outbox relay and scheduled_dispatch mover in the scheduler (leader lease).
- Dispatcher: per-channel SQS consumers, conditional claim with lease, visibility heartbeat, policy engine (defer/deny), jittered backoff via delay re-publish or scheduled_dispatch, retryable vs permanent classification, business dead-letter, poison DLQ.
- Usage records in the same tx as billable transitions (ADR-007).
- Channel registry with email (SES), SMS (**SNS** platform + Twilio BYO), Slack, webhook adapters and `MockVendorClient`.

> **Prompt:** "Plan, then implement trigger intake and the dispatcher per docs/02 and ADR-004. I want: one-transaction accept writing run + notifications + dispatch_outbox, idempotency with request-hash mismatch → 409, snapshot at accept including credential refs, outbox relay with SKIP LOCKED, per-channel SQS consumers with MessageGroupId = tenant_id, conditional claim with lease, jittered backoff (≤ 15 min via delay, longer via scheduled_dispatch), retryable vs permanent errors, and usage_records unique on notification_id. Integration tests against real Postgres + ElasticMQ: API crash after commit (relay publishes), duplicate SQS delivery (one send), consumer crash mid-send (lease expiry → redelivery), concurrent daily-SMS reservations never exceeding the limit, two tenants never seeing each other's data, and duplicate receipts never double-billing."

## Phase 4 — More channels and gRPC
- Push (FCM/APNs, BYO only), Outlook/Teams (Microsoft Graph), WhatsApp (BYO, template-message rules).
- gRPC service sharing the same service layer; buf setup.
- Vendor delivery callbacks → status updates (SES events, SNS SMS delivery-status logs, Twilio DLR, WhatsApp), `sent_unconfirmed` sweeper (72 h).

> **Prompt:** "Add the Outlook channel via Microsoft Graph behind VendorClient with a mock, and the WhatsApp channel enforcing approved-template variables in Validate. Then add the gRPC transport reusing the trigger service. Don't duplicate business logic in transports."

## Phase 5 — Observability product
- Notification timeline, run views, per-workflow/channel/tenant metrics (aggregates + rollups), failure drill-down, replay/cancel (respecting purge), tenant alerts, usage views.
- Retention: hourly payload purge job, daily partition create/drop (≈ 100 days), "payload expired" UX.

> **Prompt:** "Implement the observability read model: notification timeline, per-workflow success/failure/latency aggregates, the replay endpoint (409 payload_expired after purge), and the purge + partition jobs per ADR-006. Propose the rollup strategy first."

## Phase 6 — Console (Next.js)
- Dark ops-console UI: onboarding, apps, workflow builder, template editor with live preview (SMS segment counter), runs explorer, notification timeline, dashboards, members/roles, brand/policy (retention slider 1–7 days), **channel credentials (platform vs BYO)**, usage and daily SMS remaining, API keys, audit log.
- Server components for data, typed API client generated from OpenAPI.

> **Prompt:** "Build the console shell, auth flow, and the runs explorer with the notification timeline view in Next.js App Router (server components, Tailwind v4, dark ops aesthetic). Use the generated OpenAPI client. Handle Next 16/React 19 async params/cookies/headers correctly."

## Phase 7 — SDKs and Terraform provider
- Go, Node, Python SDKs with retries/idempotency/typed errors (incl. per-notification errors); docs and quickstarts.
- Terraform provider resources per docs/03.

## Phase 8 — Scale and production hardening
- Load tests (k6) for accept path, outbox lag and dispatch throughput; fair-queue noisy-neighbor test; per-tenant concurrency caps; autoscaling on SQS queue age.
- Terraform for AWS (ECS/RDS/ALB/SQS/SNS/SES/KMS) in ap-south-1; **flip RDS Multi-AZ, NAT per AZ, ≥ 2 tasks per service** (ADR-008) and run the forced-failover drill under load.
- SNS SMS: production access + spend quota raise for each account; platform-wide SMS cap and spend alarms.
- KMS for tenant DEKs; RLS defense in depth; backup/restore drills; SLOs and runbooks.
- Security review: SSRF on webhook channel, key handling, PII in logs and queues, credential isolation, dependency audit.
- Metering → billing provider; AWS Marketplace readiness checklist.

## Reusable prompts

**Review:** "Review the diff as a staff engineer. Look specifically for tenant leakage, missing idempotency, unbounded queries, secrets/PII in logs or queue messages, missing audit events, races in claim/lease and daily-limit reservation, and double billing. Be blunt."

**Test gap check:** "List behaviors in internal/<pkg> not covered by tests, ranked by risk. Write the top three."

**Adding a channel:** "Add channel `<name>` following the Channel/VendorClient pattern: adapter, registry entry, SQS queue + DLQ (Terraform), template schema + validation, credential modes, mock client, tests for retryable vs permanent errors, docs update."

**Schema change:** "Propose the ent schema change and migration for <X>, including indexes for the access patterns, partitioning/retention impact, encryption of sensitive columns, and impact on tenant scoping. Wait for approval before generating."

## Decisions settled (see ADRs)
1. Vendors: platform-managed **and** BYO; platform SMS via SNS with per-tenant daily limits, no DLT in v1 → ADR-005.
2. Payload retention 1–7 days (max 7), metadata 90 days, per-tenant envelope encryption, 7-day schedule horizon → ADR-006.
3. Single region ap-south-1; multi-AZ network/compute from day one, RDS Multi-AZ before prod → ADR-008.
4. Pricing per delivered notification; platform SMS bills on sent → ADR-007.
5. SQS from day one with outbox; Kafka later behind `JobQueue` → ADR-004.

## Still open
- Statutory retention period for billing records (confirm with finance before GA).
- What would justify adding Kafka (e.g. event streaming to tenants, analytics fan-out, replay); define when it comes up.
- India DLT / third-party SMS providers as platform options (post-v1 enhancement).
