# 02 — Architecture

> Updated 2026-11-27 to reflect ADR-004 (SQS + outbox), ADR-005 (credentials), ADR-006 (retention/encryption), ADR-007 (billing), ADR-008 (region/AZ).

## Planes
- **Control plane:** console (Next.js), REST admin API, Terraform provider. Manages tenants, apps, workflows, templates, brand, policy, credentials, keys, members. Low volume, strong consistency.
- **Data plane:** trigger intake (REST + gRPC + other thin transports), outbox relay, SQS, dispatcher workers, channel adapters. High volume, optimized for accept latency and throughput.
- **Observability plane:** events, metrics, traces, run/notification timelines, alerts, usage metering.

```
Client SDK / REST / gRPC / MCP / CLI / Terraform action
        │  (API key → tenant, app, env)
        ▼
 ┌───────────────┐   validate, authz, rate limit, idempotency, quotas,
 │ Trigger Intake│   platform-SMS daily limit, resolve credentials,
 └──────┬────────┘   snapshot workflow+templates+brand+policy+credential ref
        │ ONE Postgres tx: run + notifications (encrypted payload/recipients)
        │                  + dispatch_outbox | scheduled_dispatch + audit
        ▼
   PostgreSQL (system of record) ◄──── Scheduler (leader): outbox relay,
        │                               scheduled_dispatch → outbox, purge,
        │ fast-path publish after commit partitions, rollups
        ▼                               │
   SQS standard queue per channel ◄─────┘  (MessageGroupId = tenant_id → fair queues)
   + DLQ per queue (poison only)
        │ long-poll, batch 10
        ▼
 ┌───────────────┐   claim (conditional UPDATE + lease) → policy → decrypt
 │  Dispatcher   │   → render (+brand) → Channel registry → result
 └──────┬────────┘
        ▼
 Channel adapters ──► VendorClient (SES/SendGrid, SNS/Twilio, FCM/APNs, Slack, MS Graph, WhatsApp BSP, webhook)
        │
        ▼
 Status events ──► timeline, metrics, traces, webhooks-out, usage_records (billing)
        ▲
 Vendor callbacks (SES events, SNS delivery logs, Twilio DLR, WhatsApp) ──┘
```

## Trigger intake (the accept path)
1. Authenticate API key → resolve `tenant_id`, `application_id`, `environment`.
2. Validate payload and recipients against the workflow's declared schema (if any). Validate `schedule_at ≤ now + 7 days` (ADR-006).
3. Idempotency: look up `(tenant_id, idempotency_key)`. If seen with the same request hash, return the original run; different hash → `409`.
4. Enforce tenant quota and ingest rate limit (token bucket; `429` + `Retry-After`).
5. **Resolve credentials** per action: env BYO → app BYO → tenant BYO → platform (if plan/channel allow). None → that action's notifications fail with `credential_unavailable` (other actions proceed).
6. **Platform SMS daily limit:** for platform-SMS notifications, atomically reserve against `platform_sms_usage` (tenant, day). Over limit → those notifications are rejected with `platform_sms_daily_limit`; other actions proceed.
7. **Snapshot** workflow, actions, templates, brand, policy and resolved credential refs (`credential_id`, `mode`, `provider`, `billing_basis`) into the run.
8. In **one transaction**: insert `run`; one `notification` per (action × recipient) with payload/recipient **envelope-encrypted** under the tenant DEK; one `dispatch_outbox` row per immediately-due notification, or a `scheduled_dispatch` row for future ones; audit/usage-intent events.
9. After commit: best-effort `SendMessageBatch` for the new outbox rows and mark them published. Failures are left for the relay.
10. Return `202 Accepted` with `run_id` and notification ids. Never call vendors inline.

## Dispatch pipeline (ADR-004)
**Queues.** One SQS standard queue per channel (`notifex-{env}-dispatch-{channel}`), each with a DLQ. `MessageGroupId = tenant_id` enables SQS fair queues: a noisy tenant's backlog doesn't raise dwell time for others. Separate queues per channel isolate slow vendors.

**Envelope (IDs only, versioned):** `{v, tenant_id, run_id, notification_id, channel, attempt, enqueued_at, traceparent}`. No payload/PII, ever.

**Outbox relay.** Runs in the scheduler (leader via lease row). `SELECT … FROM dispatch_outbox WHERE published_at IS NULL ORDER BY id FOR UPDATE SKIP LOCKED LIMIT 100` → `SendMessageBatch` (10/call) → set `published_at`. Guarantees eventual publish after an API crash between commit and fast-path publish. Published rows are deleted after 24 h. Metric: **outbox lag** (oldest unpublished age).

**Consumer (per channel pool):**
1. Long-poll `ReceiveMessage` (wait 20 s, max 10).
2. **Claim:** `UPDATE notifications SET status='sending', attempt=attempt+1, lease_until=now()+lease WHERE id=$1 AND tenant_id=$2 AND (status IN ('queued','retrying') OR (status='sending' AND lease_until < now())) RETURNING …`. 0 rows → duplicate or done → delete message.
3. Heartbeat with `ChangeMessageVisibility` and lease extension while a send is in flight.
4. Policy evaluate → `Allow | Defer(until) | Deny(reason)`. Defer ≤ 15 min → re-publish with delay; longer → `scheduled_dispatch`; status `deferred`. Deny → `suppressed`.
5. Decrypt payload/recipient (tenant DEK) → render → `Channel.Send` with credentials fetched/decrypted now.
6. Outcome (status write and, when billable, the `usage_records` insert happen in one tx):
   - success → `sent` (sync channels may go straight to `delivered`); delete message.
   - **retryable** (5xx, timeout, 429): backoff with full jitter. ≤ 15 min → re-publish with `DelaySeconds`, status `retrying`; > 15 min → `scheduled_dispatch`. Then delete the original.
   - **permanent** (invalid recipient, auth failure, 4xx) or max attempts → `failed` with reason (business dead-letter: surfaced in console, replayable while payload exists).
   - Lifetime > 48 h in retry/deferral → `expired`.
7. SQS DLQ (`maxReceiveCount` ≈ 5) catches **poison messages only**; alarm on depth > 0.

**Duplicate sends.** At-least-once is the contract. A crash after vendor acceptance but before the status write can resend. Use `notification_id` as the vendor idempotency key where supported, and as an `Idempotency-Key` header on the webhook channel.

**Scheduled and delayed work.** `scheduled_dispatch(tenant_id, notification_id, due_at, reason)` holds anything due more than 15 min out; the scheduler moves due rows into the outbox every few seconds in batches.

**`JobQueue` seam.** `Publish(ctx, []OutboundMessage)`, `Consume(ctx, channel, handler)`, and a `Delivery` with `Ack/Extend/Retry(delay ≤ 15m)`. Only `internal/dispatch/sqsqueue` imports the SQS SDK. A Kafka implementation can be added later (partition key `tenant_id`, retry topics for delays) without touching intake or channels.

## Channel abstraction
```go
type Channel interface {
    Kind() ChannelKind
    Validate(tpl Template, rcpt Recipient) error
    Send(ctx context.Context, msg RenderedMessage, cred ResolvedCredential) (SendResult, error)
}
type VendorClient interface { /* per-channel vendor call */ }
```
- Registry maps `ChannelKind → Channel`. Adding a channel = new adapter + registry entry + template schema + tests.
- Outlook: Microsoft Graph `sendMail` via tenant-granted app permissions; Teams via Graph/webhook.
- WhatsApp: template-message rules (pre-approved templates) enforced in `Validate`.
- SMS: `Validate` caps messages at 3 segments.

## Vendor credentials (ADR-005)
- `channel_credentials` per tenant (optionally app/environment), `mode: platform|byo`. Resolution happens at accept; the reference is snapshotted and the secret is fetched at dispatch so rotations apply.
- **Platform-managed:** email via Notifex SES (shared domain, optional tenant DKIM); **SMS via Amazon SNS** (Transactional, no DLT in v1, so India goes over international routes with a numeric sender); Slack and Teams via Notifex apps installed by the tenant. Platform credentials are the dispatcher's IAM role; no stored secret.
- **BYO:** any supported provider; secrets envelope-encrypted.
- Push and WhatsApp are BYO-only at launch.
- **Platform guardrails:** sandbox tenants, per-tenant daily SMS limit (atomic reservation at accept, released if a notification fails before reaching SNS), platform-wide SMS cap below the AWS account spend quota, bounce/complaint auto-suspension, transactional-only content for platform SMS.

## Multi-tenancy
- Shared schema, `tenant_id` on every tenant-owned table; ent privacy rules + store layer enforce scoping. Postgres Row-Level Security as defense in depth.
- Per-tenant: quotas, rate limits, platform-SMS daily limit, brand, policy, credentials, payload retention (1–7 days), data encryption key.
- **Fairness:** SQS fair queues within each channel queue; per-tenant concurrency caps in the consumer as a second line; separate queues per channel.
- **Brand injection:** renderer exposes `{{brand.*}}` (logo_url, primary_color, sender_name, footer, legal) merged from tenant → application override. Email layouts wrap content with brand automatically.
- **Policy engine:** pure function `Evaluate(policy, notification, now) → Allow | Defer(until) | Deny(reason)`.
- Platform operator has a separate auth realm and access path; every access is audited.

## Data retention and encryption (ADR-006)
- **Payload/PII** (payload, raw recipients, rendered content, vendor bodies): retained 1–7 days per tenant **after terminal state**, then nulled by the hourly purge job.
- **Metadata** (statuses, timestamps, masked recipients, error codes, snapshot): **90 days**. Daily partitions on `notifications` / `notification_events` are dropped at about 100 days (90 + 7-day horizon + margin).
- **Billing records:** statutory retention.
- **Encryption:** RDS/SQS/S3 with KMS at rest; plus AES-256-GCM field encryption with per-tenant DEKs wrapped by a KMS CMK, AAD bound to `tenant_id||notification_id`. Tenant deletion destroys the DEK (crypto-shred).
- Replay is allowed only while the payload exists.

## Billing and metering (ADR-007)
- One billable unit = one notification reaching its billable state: `delivered` for most channels; **`sent` for platform SMS**. Failed, suppressed, canceled and expired notifications are not billed; async channels with no receipt after 72 h become `sent_unconfirmed` (not billed).
- `usage_records` (unique `notification_id`) is inserted in the same tx as the billable transition, so duplicates and late receipts are idempotent. Hourly rollups → `usage_daily` → billing provider.

## Scaling
- Stateless `api`, `dispatcher` and `mcp` services; `scheduler` single leader (lease row). Target deploy: ECS Fargate ARM64 in **ap-south-1**, spread over 2 AZs; RDS single-AZ until pre-prod flip to Multi-AZ (ADR-008).
- Dispatcher autoscaling per channel on `ApproximateNumberOfMessagesVisible` and `ApproximateAgeOfOldestMessage`.
- Accept path: one short Postgres tx; no queue writes inside it (the outbox makes the publish asynchronous).
- Partition hot tables by day; retention via purge job + partition drop.
- Read-heavy dashboards → rollups and read replica (Stage C).
- Backpressure: queue age drives autoscaling; quota/limit violations return `429`.
- Future: Kafka behind `JobQueue` if needed (e.g. streaming analytics, cross-region replay). Not built now.

## Observability (product feature and internal)
- **Per notification:** status timeline (`queued → deferred? → sending → retrying? → sent → delivered | sent_unconfirmed | failed | suppressed | canceled | expired`), attempts, vendor response (redacted), latency, credential mode, billing basis.
- **Per run/workflow:** per-channel success/failure breakdown, p50/p95/p99 latency, error taxonomy.
- **Per tenant/app:** volume, failure rate, quota and daily-SMS usage, billable usage, top failing workflows.
- **Replay:** re-dispatch a failed notification using its original snapshot (payload must not be purged).
- **Alerts:** tenant-configurable (failure rate, latency, vendor outage, approaching daily SMS limit) via any channel.
- **Internal:** OTel traces across intake → outbox → SQS → consumer → vendor (traceparent in envelope); Prometheus metrics (outbox lag, queue age per channel, claim duplicates, DLQ depth); structured logs with correlation ids.
- Delivery receipts: SES events, SNS SMS delivery-status logs, Twilio/WhatsApp callbacks update status via signed, idempotent callback handlers.

## Security
- API keys: random, shown once, stored as hash, scoped to app + environment, rotatable.
- OAuth + session cookies for console (CSRF protection), RBAC checks in service layer.
- Credentials and payload/PII envelope-encrypted; PII masked in logs; nothing sensitive in queue messages.
- Webhook-out and vendor-callback signatures verified; SSRF protection on webhook channel (block private IP ranges and metadata service).
- Per-tenant data export and deletion (crypto-shred) supported (GDPR/DPDP readiness).
