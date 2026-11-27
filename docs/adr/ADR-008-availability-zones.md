# ADR-008 — Region and availability zones: ap-south-1, multi-AZ-ready from day one, single-AZ data tier until prod

- **Status:** Accepted (2026-11-27)
- **Decider:** Shobhit

## Decision
- **Region:** single region **ap-south-1 (Mumbai)** for v1 (decided). Per-tenant data residency (EU etc.) is out of scope for v1; the cell model in docs/10 §13 is the future path.
- **AZs:** build network and compute **multi-AZ from the first Terraform apply**; run the **database single-AZ** until before production launch. Going fully multi-AZ is then a reviewed variable flip, not a re-architecture.

## Why not literally "single AZ for everything"
- An **ALB requires subnets in at least two AZs**, so the VPC needs 2 AZs regardless.
- Spreading ECS tasks across two private subnets costs nothing extra.
- **SQS is regional and multi-AZ inherently** (ADR-004).
- What actually costs money is **RDS Multi-AZ (~2× instance cost)** and **per-AZ NAT**; only those are deferred.

## Shape per stage
| Component | Alpha / pre-prod | Production (before launch) |
|---|---|---|
| VPC | 2 AZs (3rd reserved in CIDR plan) | 2–3 AZs |
| ALB | 2 AZs | 2–3 AZs |
| ECS (api, dispatcher, web, mcp, scheduler) | spread across 2 AZs (1 task each is fine) | ≥ 2 tasks per service, AZ-spread (scheduler: leader-elected) |
| RDS | `multi_az = false` | `multi_az = true` |
| NAT | single fck-nat | NAT Gateway per AZ (or regional NAT; evaluate) |
| SQS | regional (multi-AZ) | same |
| PgBouncer | 1 task | 2 tasks, AZ-spread |

Terraform: `var.stage` drives `rds.multi_az`, `nat.mode`, `ecs.min_tasks`.

## Is the flip quick? Yes, with caveats
- `multi_az = true` on an existing RDS instance is an **in-place modification** (AWS snapshots it and builds the standby). No data migration, but expect elevated I/O latency during conversion; apply it in a maintenance window.
- The **application must already handle failover**, and this is built from day one:
  - DB failover takes roughly 1–2 minutes (DNS flip); the pool and PgBouncer must reconnect; no pinned IPs.
  - Accept path during failover returns `503` + `Retry-After`; SDKs retry with the same idempotency key.
  - Dispatcher: claims fail, SQS messages become visible again after the timeout, and work resumes.
  - Outbox relay resumes from unpublished rows.
- **Pre-launch gate:** forced failover (`reboot --force-failover`) under k6 load in staging, verifying zero lost notifications, no duplicate billing, and accept-path recovery under 2 minutes.

## Consequences / follow-ups
- docs/10 §1, §4, §5, §13 updated; add the failover drill to docs/07 and the GA gates in ENGINEERING_CHARTER.
