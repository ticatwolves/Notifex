# 07 — Load and Performance Testing Plan

> Updated 2026-11-27 for ADR-004 (SQS + outbox), ADR-005 (daily SMS limits), ADR-006 (retention) and ADR-008 (failover gate).

Goal: prove the platform meets its SLOs under expected, peak, and abusive load, find the first bottleneck early, and keep capacity numbers honest.

## SLOs (starting points; revise with data)
| Signal | Target |
|---|---|
| Trigger accept latency | p50 < 30 ms, p95 < 60 ms, p99 < 100 ms |
| Accept availability | ≥ 99.9% monthly (error budget ≈ 43 min) |
| Outbox lag (commit → SQS publish) | p95 < 1 s, p99 < 5 s |
| Dispatch lag (due time → vendor call started) | p95 < 5 s, p99 < 30 s at steady state |
| Delivery success after retries (excluding permanent recipient errors) | ≥ 99.5% |
| Timeline freshness (event → visible in console) | p95 < 5 s |
| Console API p95 | < 300 ms |
| Data loss | zero accepted notifications lost |
| Billing correctness | zero duplicate usage records; zero billable notifications without a usage record |

## Environment
- Production-like stack in ap-south-1 staging: same instance classes (scaled by a known ratio or full-size in a short-lived env), Postgres with realistic data volume (seed ≥ 50M historical notifications across ~90 days of partitions for read-path tests), real network path through the ALB.
- **Real SQS queues** in the staging account (SQS is cheap and its behavior, including fair queues, must be measured for real). ElasticMQ is for local/CI only.
- **Mock vendor service** with scripted latency and error injection (e.g. 50–300 ms latency, 1% 5xx, 0.5% 429, occasional timeouts). Never load-test real vendors, SNS or SES; the dispatcher's SNS/SES clients point at the mock.
- Load generators separate from the system under test; k6 (scripts in `k6/`), optionally distributed.
- Observability on: Prometheus/Grafana dashboards, traces sampled, DB stats (`pg_stat_statements`, locks, wait events), SQS CloudWatch metrics incl. fair-queue `*InQuietGroups`.

## Workload model
Define from beta forecasts, then multiply:
- Peak trigger rate (per sec), average recipients/trigger, channels/workflow, payload size distribution, tenant count and skew, scheduled vs immediate mix, platform vs BYO share.
- Baseline for modeling until real data exists: 500 tenants, top 10 = 70% of volume, avg 1.5 recipients × 2 channels per trigger, 2 KB payloads, 5% scheduled, 30% platform-managed (of which 10% SMS).

## Scenarios
| Scenario | Shape | Purpose | Pass criteria |
|---|---|---|---|
| **Smoke** | 50 triggers/s (arrival rate) for 1 min, local stack with the mock vendor: `make load-smoke` | Script sanity, end-to-end correctness, CI gate | 0 errors, accept p99 within SLO, every notification `sent`/`delivered`, every billable send has exactly one usage record (`k6/verify.sql`) |
| **Load** | ramp to expected peak, hold 30 min | Validate SLOs at normal peak | All SLOs met |
| **Stress** | step up 25% every 10 min until failure | Find limits and failure mode | Degrades gracefully: 429s/backpressure, no data loss, no crash loops |
| **Spike** | 0 → 10× in 30 s, hold 5 min, drop | Burst absorption and autoscale response | Accept errors < 0.1%; queue age returns to SLO within target time |
| **Soak** | 70% of peak for 4–12 h | Leaks, bloat, vacuum, connection exhaustion | Flat memory, stable latency, no outbox/notification bloat trend |
| **Breakpoint** | ramp until SLO breach | Capacity number for planning | Documented max sustainable RPS and the bottleneck |
| **Noisy neighbor** | tenant A floods one channel; tenant B steady on same channel | SQS fair queues + consumer caps | Tenant B dispatch lag and accept latency within 10% of baseline |
| **Channel isolation** | SMS mock vendor slowed 10×; email steady | Per-channel queues | Email dispatch lag unaffected |
| **Read path** | dashboards and runs explorer under write load | Observability queries don't hurt the accept path | Console p95 within SLO; accept p99 unchanged |
| **Vendor degradation** | mock vendor slow/failing | Retry storms, queue growth | No retry amplification; backoff respected; recovery without duplicates |
| **Duplicate delivery** | inject duplicate SQS messages and visibility-timeout expiry | Claim correctness | Exactly one vendor send per notification (except crash-after-accept); one usage record |
| **Outbox recovery** | kill API tasks right after commit under load | Relay correctness | Every accepted notification published; outbox lag recovers within 30 s |
| **Daily SMS limit race** | many concurrent triggers for one tenant near its platform SMS limit | Atomic reservation | Accepted platform SMS count never exceeds the limit |
| **Failover (pre-launch gate)** | forced RDS Multi-AZ failover; kill dispatcher/API tasks; Spot interruptions | Resilience (ADR-008) | Accept recovers < 2 min; zero lost notifications; zero duplicate billing; SDK retries with same idempotency key succeed |
| **Idempotency storm** | same key from many clients | Correctness under concurrency | Exactly one run per key |
| **Retention under load** | purge job + partition drop during peak | Background jobs don't hurt hot path | Accept p99 unchanged; purge keeps up |

## What to measure
- Accept path: RPS, latency histogram, error codes, DB tx time, lock waits, fast-path publish success rate.
- Outbox: unpublished count, oldest-unpublished age, relay batch throughput.
- SQS per channel: `ApproximateNumberOfMessagesVisible`, `ApproximateAgeOfOldestMessage`, in-flight, quiet-group metrics, DLQ depth, request count (cost).
- Workers: CPU/memory, goroutines, in-flight sends, claim conflicts (duplicates), lease expirations, vendor latency/errors by channel.
- Postgres: CPU, IOPS, connections, accept tx time, claim UPDATE latency, bloat on `notifications`/`dispatch_outbox`, WAL volume, replication lag, autovacuum.
- Cost: infra cost per million notifications (incl. SQS requests) at each tested load.

## Expected bottlenecks to probe first
1. Accept tx size (run + N notifications + N outbox rows + encryption): batch inserts, DEK cache hit rate.
2. Connection pool saturation (API + workers + relay): PgBouncer; right-size pools.
3. `dispatch_outbox` churn: relay batch size, delete strategy, autovacuum settings.
4. WAL/IO from timeline events: batch event writes, daily partitions.
5. Vendor rate limits: per-vendor token buckets and per-tenant concurrency caps.
6. Dashboard queries: rollup tables, read replica.

**Decision rule for Kafka:** not a throughput question at our scale (SQS standard is effectively unlimited). Revisit Kafka only for new requirements, such as ordered per-tenant event streams to customers or analytics fan-out. Record in an ADR.

## Procedure
1. Establish baseline with smoke on every PR (fail on >10% regression).
2. Run Load weekly on staging; run the full suite before each stage gate. The failover scenario is a hard gate before production launch.
3. For each run: record commit SHA, config, dataset size, results, and dashboards snapshot in `docs/perf/YYYY-MM-DD-<scenario>.md`.
4. Fix the first bottleneck, re-run, repeat. Never tune several things at once.
5. Publish a capacity model: RPS per API task, messages/sec per worker per channel, DB headroom, and the scaling triggers (CPU, queue age, outbox lag).

## Autoscaling and protection
- Scale API on CPU/RPS; dispatcher pools per channel on SQS queue age and visible messages.
- Per-tenant ingest rate limits, quotas and daily SMS limits return `429` (or per-notification errors) with `Retry-After`; global load shedding protects the platform when saturated.
- Alerts: accept p99, error rate, outbox lag, queue age per channel, DLQ depth > 0, DB connection use, replication lag, disk growth forecast, SMS spend vs AWS quota.

## Reporting template
Scenario, date, commit, environment, workload, results vs SLO (table), bottleneck analysis with graphs, changes made, residual risk, recommended capacity and next test.

See `k6/trigger-load.js` and `k6/noisy-neighbor.js` (both trigger the seeded `order_shipped` workflow; no remote imports, so a run never fetches code). `make load-smoke` runs the smoke locally and verifies the database afterwards; `make load-test BASE_URL=... API_KEYS=... SCENARIO=load|stress|spike|soak` runs the others against a target. The k6 image is pinned by digest in the Makefile. The noisy-neighbor script should target a single channel so fair-queue behavior is what's measured.

Local numbers are a regression baseline, not capacity: everything shares one laptop (load generator, API, dispatcher, Postgres, ElasticMQ). Capacity numbers come from staging (Phase 8).
