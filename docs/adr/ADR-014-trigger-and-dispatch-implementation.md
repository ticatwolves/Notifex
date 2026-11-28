# ADR-014 — Trigger intake and dispatch: implementation decisions

- **Status:** Accepted (2026-11-28). Approved with the Phase 3 plan.
- **Decider:** Shobhit
- **Builds on:** ADR-004 (SQS + outbox), ADR-005/013 (credentials), ADR-006 (retention, encryption), ADR-007 (billing). Nothing here changes those decisions; this ADR fixes the details they leave open.

## 1. Partitioned notification tables
- `notifications` and `notification_events` are **range-partitioned by day** on `created_at` (`<table>_pYYYYMMDD`, UTC, no DEFAULT partition; helpers in `internal/db/partition`). The scheduler keeps 14 days of partitions ahead.
- ent can't express `PARTITION BY`, so these tables' DDL is **hand-written** in their migration and their ent schemas carry `entsql.Annotation{Skip: true}` (ent code is generated; the migration generator ignores them).
- The primary key must contain the partition key: `PRIMARY KEY (id, created_at)`.
- **`created_at` is the UUIDv7 id's timestamp** (millisecond precision), set by the store at insert. A lookup by id alone (the queue envelope carries ids only) computes `created_at` from the id and hits exactly one partition.

## 2. Config snapshots (deduplicated)
- At accept, the run's configuration is snapshotted: the workflow (key, name, payload schema), the enabled actions narrowed by `channels`, **all locales** of their templates, the effective brand and policy, and each action's resolved credential reference (`credential_id`, `mode`, `provider`, `billing_basis`).
- The snapshot is stored once in **`config_snapshots (tenant_id, hash, doc, created_at)`**, with primary key `(tenant_id, hash)` and `hash = SHA-256 of the canonical JSON`. Runs reference `(tenant_id, snapshot_hash)`.
- Snapshots are **immutable** (no update path in the store), are never listed or exposed as versions, and are deleted by the retention job once no run references them. This is a content-addressed cache of accepted runs' inputs, not a versioning feature, so the "no workflow_versions/template_versions" rule stands. Approved explicitly (Phase 3 plan, decision 2).
- Why: inline snapshots would be 20–60 KB per run. At 10M runs a day that is hundreds of GB a day of identical copies.

## 3. Accept path
1. API key → tenant, application, environment (existing authenticator).
2. Per-key ingest rate limit (per process, §8). `429` + `Retry-After`.
3. Validate the request: workflow by key in the key's application, **must be `active`** (`404 workflow_not_found` if missing, `409 workflow_not_active` if draft or archived). `channels` may only narrow: a channel the workflow doesn't have is `400`. `schedule_at` at most now + 7 days (`400 schedule_horizon_exceeded`); a past `schedule_at` is treated as now. The payload is validated against the workflow's payload schema (compiled once per workflow version of `updated_at`, cached).
4. **Idempotency (optional header, kept 24 h):** scope `(tenant_id, key)`, request hash = SHA-256 of the canonical body. The record is inserted **in the accept transaction**; a concurrent duplicate hits the unique index, waits for the first transaction, and returns the original run. Same key with a different hash is `409 idempotency_conflict`.
5. Per (action × recipient), decide the notification's initial state, each independently (actions never block each other):
   - policy disallows the channel → `failed`, `channel_not_allowed`;
   - recipient has no address for the channel → `failed`, `recipient_missing`;
   - no template for the recipient's locale chain (ADR-012 §7) → `failed`, `template_missing`;
   - credential resolution fails → `failed`, `credential_unavailable`;
   - platform SMS: atomic reservation (`UPDATE platform_sms_usage SET count = count + 1 WHERE tenant_id=$1 AND day=$2 AND count + 1 <= $limit`, row upserted per tenant per UTC day; the platform-wide cap is a second counter) fails → `failed`, `platform_sms_daily_limit` with `retry_after` = next UTC midnight;
   - otherwise `queued` (or `deferred` with a `scheduled_dispatch` row if `schedule_at` > now + 15 min, or an outbox row with a delay if it's within 15 min).
6. **One transaction:** config snapshot (insert if absent), `run` (payload encrypted), `notifications` (recipient encrypted, masked, hashed), `dispatch_outbox` / `scheduled_dispatch` rows, idempotency record, SMS reservations.
7. After commit: fast-path publish of the new outbox rows; mark them published. A failure leaves them for the relay.
8. Response: **always `202`** for a valid request, with every notification's state. If no notification was accepted, the run is created with status `failed`; the per-notification errors say why. (This refines docs/03, which had the request "fail as a whole" in that case: a stored run is idempotent and visible in the timeline, and clients read per-notification errors either way.)

## 4. Encryption and recipient data (ADR-006)
- `runs.payload` is encrypted with AAD `(tenant, run_id, "payload")`; `notifications.recipient` with `(tenant, notification_id, "recipient")`; `rendered_content` and `vendor_response` likewise per notification.
- `recipient_masked` uses the existing masking (`a***@d***.com`, `+91******1234`).
- **`recipient_hash` = HMAC-SHA256(`RECIPIENT_HASH_KEY`, normalized address)**, a new secret separate from the API key pepper; dev default in `local` only.

## 5. Dispatch
- **Envelope v1** (ADR-004), queues `notifex-{env}-dispatch-{channel}` + DLQ each, `MessageGroupId = tenant_id`.
- **Claim** is the ADR-004 conditional update (`queued|retrying|deferred`, or `sending` with an expired lease). `deferred` is included because a deferred notification is re-published when its time comes and must be claimable then. Lease 60 s, renewed every 20 s with `ChangeMessageVisibility` while sending. Zero rows → ack and drop.
- **Policy at dispatch:** quiet hours → `deferred` until the window ends (SQS delay if ≤ 15 min, else `scheduled_dispatch`); per-tenant channel rate limit (per process) → short defer.
- **Backoff:** full jitter, base 10 s, factor 2, cap 6 h; `max_attempts` from the snapshot's policy (default 5). Anything still not terminal **48 h** after creation becomes `expired`.
- **Classification:** timeouts, 5xx, 429 and connection errors are retryable; 4xx (except 408/429), invalid recipient and auth failures are permanent. Vendors report through `SendResult{Status, Retryable, VendorMessageID, Code}`.
- **Duplicate sends:** `notification_id` is the vendor idempotency key where supported, and the webhook channel's `Idempotency-Key` header.
- A notification that fails permanently **before** reaching SNS releases its platform-SMS reservation.
- Consumers are off unless `DISPATCH_ENABLED=true` (dark launch).

## 6. Billing until delivery receipts exist (ADR-007; receipts in Phase 4)
- `billing_basis` is fixed at accept from the resolved credential (`sent` for platform SMS, `delivered` otherwise).
- Slack and webhook are synchronous: success → `delivered` → billable. Platform SMS → billable at `sent`. **BYO email and SMS (and platform email) stop at `sent` and are not billed until Phase 4 receipts move them to `delivered`.**
- `usage_records` is unique per `notification_id` and written in the same transaction as the billable transition.

## 7. Scheduler
- One leader at a time, via a `scheduler_leases` row (`name`, `holder`, `expires_at`) renewed every 5 s, 15 s lease. Jobs: outbox relay (`FOR UPDATE SKIP LOCKED`, batches of 100, `SendMessageBatch` of 10), scheduled-dispatch mover, partition pre-creation, published-outbox cleanup after 24 h. Purge and rollups arrive in Phase 5.

## 8. Rate limits (per process for now)
Ingest per API key and per-tenant channel limits are token buckets **per process**; the effective limit is the configured value times the number of tasks, and that is documented. Shared counters come after load tests measure the need (a new store such as Redis needs approval first).

## 9. Dependencies
`github.com/aws/aws-sdk-go-v2` (`sqs`, `sesv2`, `sns`, `sts`, `config`). SQS types only in `internal/dispatch/sqsqueue`; SES, SNS and STS only in `internal/channel/vendor`. Other vendors use `net/http` and `net/smtp`.

## 10. Limits
Up to **1,000 recipients** per trigger (contract). Slice 7's load smoke test measures accept p99 with it; if it regresses the budget, the synchronous limit drops to 100 and a batch path is added.

## Consequences
- New tables: `config_snapshots`, `runs`, `notifications` (partitioned), `notification_events` (partitioned), `dispatch_outbox`, `scheduled_dispatch`, `idempotency_records`, `platform_sms_usage`, `usage_records`, `scheduler_leases`.
- New config: `RECIPIENT_HASH_KEY`, `DISPATCH_ENABLED`, SQS endpoint and queue prefix, platform SMS cap.
- New error codes: `workflow_not_active`, `recipient_missing`, `template_missing`.

## Alternatives considered
- **Inline snapshot per run.** Storage-prohibitive at volume (§2). Rejected.
- **Unpartitioned notifications, partition later.** Converting a large live table is a long, risky migration. Rejected.
- **Failing the whole request when nothing is accepted.** Not idempotent-friendly and hides per-notification reasons. Rejected (§3.8).
- **Redis for rate limits and idempotency now.** No measured need; new infrastructure needs approval. Deferred.

## Amendment (2026-11-28, Phase 3 slice 2): data model as built
- **Migrations:** the ten ent-managed tables come from the generator (`20261128033649_trigger_and_dispatch`). The partitioned tables are hand-written (`20261128033700_notifications_partitioned`), including a `DO` block that creates partitions from yesterday through 14 days ahead so a fresh database accepts writes before the scheduler runs. The generator gained a diff hook that ignores `schema.ManualTables` and their `_pYYYYMMDD` partitions, and `make migration-rehash` rewrites `atlas.sum` after a hand-written file is added.
- **Partitioned table keys:** `PRIMARY KEY (id, created_at)`; the store computes `created_at` from the UUIDv7 id on every read and write, so each lookup touches one partition. A run's notifications are listed within an hour of the run's own id time. A row beyond partition coverage fails (tested).
- **No foreign keys from runs or notifications to configuration** (environments, workflows, actions): runs are history and outlive what they were accepted with. The only foreign keys are to `tenants` and, for runs, to their snapshot `(tenant_id, snapshot_hash)`.
- **Platform-SMS counters:** `platform_sms_usage` (per tenant per day) and `platform_sms_totals` (platform-wide). Each reservation is an upsert of the day's row followed by one conditional increment; 20 concurrent reservations against a limit of 7 succeed exactly 7 times (tested, mutation-checked).
- **Cross-tenant operations** (outbox claim, scheduled-dispatch take, expired idempotency cleanup, leases) are raw SQL with explicit columns; tenant-scoped ones go through ent with the tenant scope. Two relays claiming concurrently get disjoint rows (`SKIP LOCKED`, tested).

## Amendment (2026-11-28, Phase 3 slice 3): trigger intake as built
- **API key scopes are enforced on the data plane.** A key with no scopes keeps full access (keys created before enforcement keep working); a key with scopes needs `trigger` for `POST /v1/trigger` and `read` for `GET /v1/runs/{id}` and `GET /v1/notifications/{id}` (`403 forbidden` otherwise).
- **Recipient addresses per channel:** email needs `email`, SMS needs `phone`, WhatsApp needs `whatsapp` (`recipient_missing` otherwise). Slack and webhook don't require one: Slack can post to the credential's channel (or DM `slack_user`), and the webhook channel posts to the tenant's endpoint. The whole recipient object is encrypted per notification, because templates can reference any recipient field (ADR-012 §1).
- **New tenants have `platform_sms_daily_limit = 0`** (Phase 1 default), so platform SMS is refused (`platform_sms_daily_limit`) until an operator sets a limit. Sandbox tenants get a low limit from the operator.
- **`source`** is `rest` for the REST transport and defaults to `rest` when a caller leaves it empty; other transports set their own.
- **Fast path:** the API has no publisher until slice 4, so the outbox relay publishes accepted work. The service already marks fast-path publishes and falls back to the relay on any failure.
- **Open for slice 6:** the webhook channel's endpoint URL isn't in the Phase 2 contract (the `webhook/signing` credential has no `url`, and webhook templates have only `body`). Slice 6 must decide where it lives (recommendation: a required `url` in the `webhook/signing` credential config, per environment) before the adapter is built.

## Amendment (2026-11-28, Phase 3 slice 4): queue and scheduler as built
- **`internal/dispatch`** holds the envelope (v1, ids only, decoded with unknown fields rejected so nothing can be smuggled through the queue), `JobQueue`/`Delivery`, `OutboxPublisher` (the API's after-commit fast path) and trace helpers: the trigger writes the request's W3C `traceparent` into outbox and scheduled rows, and consumers continue the trace from the envelope.
- **`sqsqueue`:** `SendMessageBatch` in tens, any failed entry fails the call (the rows stay for the relay); long-poll receive never asks for more messages than there are free handler slots; undecodable messages are left alone so the redrive policy moves them to the DLQ; `Retry` publishes attempt+1 with a delay, then deletes the original. `EnsureQueues` creates queues and DLQs for local use and tests (Terraform owns them in AWS). ElasticMQ accepts `MessageGroupId` on standard queues, so fair queues are on locally too (`SQS_FAIR_QUEUES`, default true). Queue names come from `QUEUE_PREFIX` (default `notifex-<env>-dispatch-`).
- **Scheduler:** leader election through the `scheduler` lease (renewed every 5 s, 15 s TTL, released on graceful stop). The relay locks up to 100 rows with `SKIP LOCKED`, publishes, marks them published and commits; a publish failure rolls back. The mover takes due `scheduled_dispatch` rows and enqueues them in the same transaction. Maintenance keeps partitions 14 days ahead and deletes published outbox rows after 24 h and expired idempotency records.
- **Tested against ElasticMQ:** publish/consume beyond one batch; delays and the 15-minute cap; retry incrementing the attempt; redelivery after the visibility timeout and `Extend` holding a message; duplicates delivered twice (the claim makes them harmless); poison messages reaching the DLQ without touching the handler; relay after an API crash; three concurrent relays publishing 250 rows exactly once; leader handover on stop and on lease expiry.
- **Test note:** the relay, mover and lease are cross-tenant by design, so the scheduler package's tests run sequentially against their shared test database.

