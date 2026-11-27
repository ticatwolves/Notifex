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

## Purge mechanics
- `notifications` and `notification_events` partitioned **daily** on `created_at`.
- **Payload retention clock starts at the notification's terminal state** (sent/delivered/failed/cancelled/expired), so retries and deferrals never lose their payload mid-flight. Max retry/deferral window is 48 h, after which the notification becomes `expired`.
- An hourly purge job nulls sensitive columns (`payload = NULL, purged_at = now()`) once a row passes its tenant's retention after terminal state, in bounded batches. This enforces the 1–7-day promise.
- **Partitions are dropped after 90 days + 7-day schedule horizon + margin (≈ 100 days)**, removing metadata. By then sensitive columns are long since NULL.
- Pre-send payloads for scheduled triggers live encrypted in `scheduled_dispatch` and are deleted once moved to the outbox.
- Replay is only possible while the payload exists; after purge the console shows "payload expired" and disables replay, but the metadata timeline stays visible for 90 days.

## Notes
- Backups (PITR 7–14 days) can hold payloads longer than the tenant's retention. Encryption plus crypto-shred covers deletion requests; disclose this in the DPA.
- 90 days of `notifications` + `notification_events` is the dominant storage driver; size RDS storage from load-test volumes (docs/07) and move cold metadata reads to the replica at Stage C.

## Consequences / follow-ups
- Policy entity gains `payload_retention_days` (1–7). Audit event on change. Trigger API validates `schedule_at ≤ now + 7d`.
- docs/03, docs/02 §Security, docs/06 (decryption isolation tests, purge-job tests, "no PII after retention" test), docs/10 (KMS key, partition jobs, storage sizing).
