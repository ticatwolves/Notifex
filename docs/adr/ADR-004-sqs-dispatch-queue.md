# ADR-004 — SQS as the dispatch queue from day one (Kafka-ready seam)

- **Status:** Accepted (2026-11-27)
- **Supersedes:** the "Postgres `SKIP LOCKED` dispatch queue" locked decision in CLAUDE.md, docs/02 §Dispatch pipeline, docs/10 §1 (Queue row) and §11 (Stage D trigger).
- **Decider:** Shobhit

## Decision
Use **Amazon SQS standard queues** for dispatch jobs from the first release. PostgreSQL stays the **system of record** for runs, notifications, attempts and state. Kafka is **not** built now, but the `JobQueue` seam and the message envelope are designed so a Kafka implementation can be added without touching trigger intake or channel code.

## Why
- Removes the queue-contention ceiling on Postgres and the autovacuum pressure of a hot `dispatch_jobs` table.
- **SQS fair queues** (standard queues + `MessageGroupId`) give per-tenant noisy-neighbour mitigation natively, which the Postgres design had to build by hand. Setting `MessageGroupId = tenant_id` is all that is required; consumers don't change.
- SQS is regional and multi-AZ by design, so the queue is already HA while the DB is single-AZ (see ADR-008).
- Spot interruption of dispatchers is harmless: unacked messages reappear after the visibility timeout.

## What we give up, and how we compensate
| Lost property | Compensation |
|---|---|
| One-transaction accept (run + jobs in the same DB tx) | **Transactional outbox**: accept tx writes `run`, `notifications`, and `dispatch_outbox` rows. A relay publishes to SQS. |
| Exactly-one claim via row lock | Consumer-side **conditional state transition** in Postgres is the real claim; SQS duplicates are expected and harmless. |
| Arbitrary `run_at` delays | SQS `DelaySeconds` max is **15 min**. Longer delays (scheduled triggers, quiet-hours deferral, long backoff) live in a Postgres `scheduled_dispatch` table; the scheduler moves due rows into the outbox. |
| SQL visibility into the queue | Queue depth/age from CloudWatch (`ApproximateNumberOfMessages*`, `ApproximateAgeOfOldestMessage`, fair-queue `*InQuietGroups` metrics); per-notification state stays queryable in Postgres. |

## Design
**Accept path (unchanged contract, 202 Accepted):**
1. One Postgres tx: `run` (+ snapshot), `notification` rows, `dispatch_outbox` rows (`notification_id`, `queue`, `delay_s`, `published_at NULL`), usage/audit event.
2. After commit, the API *attempts* a fast-path `SendMessageBatch` and marks those outbox rows published. Failure here is fine.
3. **Outbox relay** (in the scheduler service, leader via lease row) sweeps unpublished rows with `FOR UPDATE SKIP LOCKED`, batches of 10, and marks them published. It guarantees eventual publish; a duplicate publish is possible and tolerated.

**Message envelope (transport-agnostic, versioned):**
```json
{"v":1,"tenant_id":"…","run_id":"…","notification_id":"…","channel":"email",
 "attempt":1,"enqueued_at":"…","traceparent":"…"}
```
- **No payload, recipient or rendered content in the message.** The worker loads them from Postgres (decrypting per ADR-006). This keeps PII inside the 7-day-retention, envelope-encrypted store and makes the message size limit irrelevant. (SQS now allows up to 1 MiB, but we don't rely on it.)
- `MessageGroupId = tenant_id` (fair queues). The same field becomes the **Kafka partition key** later.
- Queues are SSE-KMS encrypted anyway.

**Queues:** one standard queue per channel kind (`notifex-{env}-dispatch-{channel}`) + one DLQ each, via Terraform `for_each`. Each channel gets its own consumer pool sized to the vendor's rate limits, so a slow SMS vendor can't starve push. Fair queues handle fairness between tenants *within* a channel.

**Consumer:**
1. Long-poll `ReceiveMessage` (20 s, batch 10).
2. Claim: `UPDATE notifications SET status='sending', attempt=attempt+1, lease_until=now()+$lease WHERE id=$1 AND tenant_id=$2 AND (status IN ('queued','retrying') OR (status='sending' AND lease_until < now())) RETURNING …`. Zero rows means duplicate or already done: delete the message, done.
3. Heartbeat: extend visibility (`ChangeMessageVisibility`) while a send is in flight; visibility timeout > p99 vendor latency.
4. Result:
   - success → status `sent`/`delivered`, usage record (ADR-007), `DeleteMessage`.
   - retryable, backoff ≤ 15 min → re-publish with `DelaySeconds` (attempt+1), then delete the original.
   - retryable, backoff > 15 min → insert into `scheduled_dispatch`, then delete.
   - permanent / max attempts → `failed` (business dead-letter, visible in console, replayable).
5. **SQS DLQ (`maxReceiveCount` ≈ 5) is for poison messages only** (crashes, undecodable envelopes), not business failures. Alarm on DLQ depth > 0.

**Duplicate sends:** at-least-once remains the contract. A crash after the vendor accepted but before the status write can resend. Where the vendor supports it, pass `notification_id` as the vendor idempotency key; for the webhook channel, send it as an `Idempotency-Key` header.

## `JobQueue` seam (Kafka room)
```go
type Envelope struct { V int; TenantID, RunID, NotificationID string; Channel ChannelKind; Attempt int; EnqueuedAt time.Time; TraceParent string }

type JobQueue interface {
    Publish(ctx context.Context, msgs []OutboundMessage) error          // OutboundMessage{Envelope, Delay ≤ 15m}
    Consume(ctx context.Context, channel ChannelKind, h Handler) error  // blocks; honours ctx cancel
}
type Delivery interface {
    Envelope() Envelope
    Ack(ctx context.Context) error
    Extend(ctx context.Context, d time.Duration) error
    Retry(ctx context.Context, delay time.Duration) error // impl decides: SQS delay / Kafka retry topic
}
```
Rules that keep Kafka possible: no SQS types outside `internal/dispatch/sqsqueue`; delays only through `Retry(delay)` and capped at 15 min (anything longer goes through `scheduled_dispatch`); ordering is never assumed; the partition/group key is always `tenant_id`.

## Dev and test
- docker-compose adds **ElasticMQ** (SQS-compatible) or LocalStack; integration tests run against it.
- An `inmem` `JobQueue` serves unit tests.
- Required tests: duplicate delivery (same notification twice → one send), crash after claim (lease expiry → redelivery), outbox relay after API crash post-commit, fair-queue smoke test with a noisy tenant (re-point `noisy-neighbor.js`).

## Cost
SQS standard is billed per request (list price roughly $0.40 per million in us-east-1 after the 1M/month free tier; verify for ap-south-1). Expect about 3–5 requests per notification (send, receive, delete, occasional visibility extension). Add an **SQS interface VPC endpoint** once NAT data-processing charges for SQS traffic exceed the endpoint's hourly cost.

## Consequences / follow-ups
- Update CLAUDE.md "Dispatch" and "Queue seam" lines, docs/02, docs/10 (queue row, Stage D, Terraform `sqs-dispatch/` module, alarms), docs/07 (load test now measures outbox lag + SQS age), docs/04 Phase 3 prompt. *(Done 2026-11-27.)*
- New tables: `dispatch_outbox`, `scheduled_dispatch`. Drop `dispatch_jobs`.
