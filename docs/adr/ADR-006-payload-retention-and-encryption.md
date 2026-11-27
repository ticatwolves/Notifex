# ADR-006 — Payload retention (≤ 7 days, configurable) and encryption at rest

- **Status:** Accepted (2026-11-27)
- **Decider:** Shobhit

## Decision
- **Retention for sensitive data is configurable per tenant from 1 to 7 days; default 7, hard maximum 7.** The policy service rejects values outside the range; the maximum is a platform constant, not a plan setting.
- **Notification metadata is retained for 90 days** (decided). Billing/usage records follow statutory requirements (confirm the period with finance before GA).
- **Sensitive data is encrypted at rest at two layers:** storage (RDS/S3/SQS with KMS) and application-level envelope encryption of sensitive columns with a **per-tenant data key**.
- **Scheduled triggers are capped at 7 days in the future.** Requests beyond the horizon are rejected with `400 schedule_horizon_exceeded`.

## What is purged when
| Purged after tenant retention (1–7 days) | Kept 90 days (no PII) | Kept per statute |
|---|---|---|
| Trigger payload, raw recipient details, rendered subject/body, vendor request/response bodies | Run/notification metadata: ids, workflow, channel, statuses, timestamps, attempts, error codes, latency; **masked** recipient; recipient keyed-HMAC hash; workflow/template snapshot | `usage_records`, `usage_daily`, invoices (ADR-007) |

Aggregated metrics/rollups (per workflow/channel/day, no identifiers below tenant/workflow level) may be kept longer for dashboards.

## Encryption design
- Layer 1: RDS storage encryption, S3 SSE-KMS, SQS SSE-KMS, encrypted backups/snapshots (all CMK).
- Layer 2: columns `payload`, `recipient`, `rendered_content`, `vendor_response` stored as AES-256-GCM ciphertext. Per-tenant DEK wrapped by a KMS CMK (`notifex-{env}-tenant-data`); unwrapped DEKs cached in memory with a short TTL and size bound; AAD = `tenant_id || notification_id`, so a ciphertext copied across rows or tenants fails to decrypt.
- `KeyProvider` interface (KMS in AWS, local key in dev), shared with ADR-005 credential encryption.
- **Tenant deletion = crypto-shred:** destroy the tenant DEK and every payload ciphertext, including copies in backups, becomes unreadable.
- Logs, traces and metrics never contain payload or raw recipients.

### Implementation notes (2026-11-27, Phase 0 slice 2: `internal/crypto`)
These refine the encryption design above without changing the decision:
- **Per-cell key derivation.** Each value is encrypted with AES-256-GCM under `HKDF-SHA256(DEK, info = "notifex/field/v1" | tenant_id | row_id | column)`, not under the DEK directly. Random 96-bit GCM nonces are only safe for about 2^32 messages per key. A busy tenant writing about 4 encrypted columns per notification at 10M notifications/day would reach that on a single DEK in about 100 days. A per-cell key encrypts only a handful of values. The cost is about 1.2 µs per 1 KiB field (benchmarked).
- **The AAD includes the column name** (`tenant_id | row_id | column`), so `payload` and `recipient` ciphertexts can't be swapped within a row. The header bytes are authenticated too.
- **Ciphertext format v1:** `0x01 | dek_version | nonce(12) | ciphertext+tag`. `dek_version` is always 1 until DEK rotation exists; rotation needs no format change.
- **Wrapped DEKs are bound to their tenant** through the KMS encryption context `{purpose: tenant-dek, tenant_id}`. A wrapped DEK copied onto another tenant row will not unwrap, and every unwrap is attributable in CloudTrail.
- **Cache:** unwrapped DEKs sit in a bounded LRU (default 10k tenants, 5-minute TTL) with singleflight on misses. Evicted keys are zeroed, best effort. **Crypto-shred is immediate on the instance that runs it, and takes effect within one TTL on other instances.** Tenant deletion must also stop processing for that tenant (status `deleted`) so nothing is newly encrypted during that window.
- An encrypted empty value decrypts to an empty value, never nil, so it stays distinct from a purged (NULL) column.

## Purge mechanics
- `notifications` and `notification_events` partitioned **daily** on `created_at`.
- **Payload retention clock starts at the notification's terminal state** (sent/delivered/failed/cancelled/expired), so retries and deferrals never lose their payload mid-flight. Max retry/deferral window is 48 h, after which the notification becomes `expired`.
- An hourly purge job nulls sensitive columns (`payload = NULL, purged_at = now()`) once a row passes its tenant's retention after terminal state, in bounded batches. This enforces the 1–7-day promise.
- **Partitions are dropped after 90 days + 7-day schedule horizon + margin (≈ 100 days)**, removing metadata. By then sensitive columns are long since NULL.
- Pre-send payloads for scheduled triggers live encrypted in `scheduled_dispatch` and are deleted once moved to the outbox.
- **Partition helpers (`internal/db/partition`, Phase 0 slice 4):**
  - Partitions are named `<table>_pYYYYMMDD` with UTC day bounds `[d, d+1)`. There is no DEFAULT partition, so a row beyond coverage fails loudly. `Coverage()` feeds a "days of partitions ahead" alert (Phase 5). Partitions are pre-created about 14 days ahead.
  - Every DDL statement runs under `lock_timeout` (5s), so partition maintenance can't queue behind a long transaction and then block the accept path. A run that times out is retried.
  - Drops use `DETACH PARTITION … CONCURRENTLY`, then `DROP`. Detaches left pending by a crash are finalized, and leftovers detached but not dropped are cleaned up. Bounds come from the catalog, not from names.
  - A hard `MinRetention` guard (90 days) refuses to drop anything newer, whatever cutoff the caller passes.
- Replay is only possible while the payload exists; after purge the console shows "payload expired" and disables replay, but the metadata timeline stays visible for 90 days.

## Notes
- Backups (PITR 7–14 days) can hold payloads longer than the tenant's retention. Encryption plus crypto-shred covers deletion requests; disclose this in the DPA.
- 90 days of `notifications` + `notification_events` is the dominant storage driver; size RDS storage from load-test volumes (docs/07) and move cold metadata reads to the replica at Stage C.

## Consequences / follow-ups
- Policy entity gains `payload_retention_days` (1–7). Audit event on change. Trigger API validates `schedule_at ≤ now + 7d`.
- docs/03, docs/02 §Security, docs/06 (decryption isolation tests, purge-job tests, "no PII after retention" test), docs/10 (KMS key, partition jobs, storage sizing).
