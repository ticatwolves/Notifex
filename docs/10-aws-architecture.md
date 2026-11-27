# 10 — AWS Architecture: Cost-Effective Start, Built to Scale

> Updated 2026-11-27 for ADR-004 (SQS + outbox), ADR-005 (SNS platform SMS, SES), ADR-006 (KMS tenant keys, retention), ADR-008 (ap-south-1, AZ strategy).

Stack: **Next.js** (console, separate deployable), **Go** (core platform), **Python** (insights/batch/tooling), **PostgreSQL**, **SQS**, deployed on **AWS ap-south-1** with **Terraform**.
Design rule: *start with the fewest moving parts that are still safe, and put a clearly marked seam wherever we expect to scale*. Every upgrade below is triggered by a measured signal, not by anxiety.

## 1. Decisions at a glance
| Area | Start (Stages A–B) | Scale later (Stages C–D) | Why |
|---|---|---|---|
| Region | **`ap-south-1` (Mumbai) only** (ADR-008) | DR region (e.g. `ap-south-2`) for backups/warm standby; data residency via cells | Indian latency/residency; Mumbai list prices are typically 10–30% above us-east-1 |
| AZs | **VPC, ALB, ECS across 2 AZs from day one**; RDS single-AZ | RDS Multi-AZ + NAT per AZ **before production launch**; 3rd AZ later | ALB needs 2 AZs anyway; only RDS Multi-AZ and per-AZ NAT cost real money |
| Compute | **ECS on Fargate, ARM64 (Graviton)** | Compute Savings Plans; EKS only if forced by ecosystem needs | No servers to patch, scale per task, ~20% cheaper on ARM, no EKS control-plane fee |
| Workers | Fargate **Spot** for dispatcher (safe: SQS visibility timeout + leases + at-least-once) | mixed on-demand floor + Spot burst | Up to ~70% discount on interruption-tolerant work |
| Database | **RDS PostgreSQL** (t4g), gp3, single-AZ → Multi-AZ pre-prod | bigger instance, read replica, then Aurora if needed | System of record; predictable cost |
| Queue | **SQS standard, one queue + DLQ per channel, fair queues (`MessageGroupId = tenant_id`), Postgres outbox** (ADR-004) | Kafka behind `JobQueue` only for new requirements | Removes Postgres queue contention; native noisy-neighbor mitigation; regional and multi-AZ |
| Messaging vendors (platform-managed) | **SES** (email), **SNS** (SMS, Transactional, no DLT in v1) | DLT local routes / End User Messaging v2 API, more providers | Native, IAM-auth, no stored secrets (ADR-005) |
| Frontend | Next.js container on Fargate behind CloudFront | OpenNext/Lambda or Amplify if cost-minimizing | Same deploy model as backend |
| Edge | CloudFront + ALB + WAF managed rules | stricter WAF, Shield Advanced only if needed | Cheap baseline protection |
| Egress | **NAT instance (fck-nat) → NAT Gateway per AZ before prod** | regional NAT, VPC endpoints (SQS, SNS, SES, ECR, Logs) | NAT Gateway ≈ $33/mo + $0.045/GB; instance is a few dollars |
| Secrets/crypto | SSM + Secrets Manager + **KMS CMKs incl. `tenant-data` key for per-tenant DEKs** | rotation, key-per-tier if needed | Envelope encryption of credentials and payloads (ADR-005/006) |
| Observability | OTLP → Grafana Cloud free tier (verify limits) + CloudWatch with short retention | managed Prometheus + Tempo/Loki, or Datadog | CloudWatch Logs ingestion is $0.50/GB, so keep logs lean |
| IaC/CI | Terraform, GitHub Actions + OIDC | blue/green deploys, ephemeral PR environments | No long-lived AWS keys |

Alternatives rejected for now: EKS (control-plane cost + ops overhead), Lambda-only (cold starts, DB connection pressure, awkward gRPC), App Runner (limited networking/gRPC control), single EC2 with docker-compose (no HA/scaling path), Postgres `SKIP LOCKED` as the dispatch queue (superseded by ADR-004), Kafka/MSK now (cost and ops without a requirement).

## 2. Accounts and environments
- AWS Organizations: `management` (billing/SSO only), `staging`, `prod`; later `security/logging`. Humans via IAM Identity Center; CI via GitHub OIDC roles; **no long-lived access keys**.
- Alpha can run one workload account; **split staging/prod before real customer data** (Stage B).
- **Per workload account, request SMS production access and a spend-quota increase** (SNS/End User Messaging sandbox is the default for new accounts), and verify the SES sending domain + production access.
- Staging is a smaller copy of prod from the same Terraform modules; scale ECS to zero and stop RDS on nights/weekends.
- Local dev: docker-compose (Postgres, **ElasticMQ**, mock vendors, all services). No cloud needed day to day.

## 3. Target architecture (Stage B shape)
```
Users ──► Route53 ──► CloudFront ──► (WAF) ──► ALB (2 AZ) ──┬─► web  (Next.js, Fargate)
Tenant backends / SDKs ──► api.<domain> (REST+gRPC) ─────────┤
AI tools ──► mcp.<domain> ───────────────────────────────────┼─► mcp  (Go, stateless)
                                                             ├─► api  (Go core, gRPC target group)
 private subnets (2 AZ):                                     │
   api ──► PgBouncer ──► RDS PostgreSQL (encrypted; Multi-AZ before prod)
   api ──► SQS (fast-path publish of outbox rows)
   scheduler (1 leader) ──► outbox relay ──► SQS dispatch-{channel} queues (+ DLQs)
                        ──► scheduled_dispatch mover, purge job, partitions, rollups
   dispatcher pools per channel (Fargate Spot + OD) ◄── SQS long-poll
        ──► PgBouncer/RDS (claim, status, usage)
        ──► KMS (unwrap tenant DEKs, cached)
        ──► SES / SNS (platform) via VPC endpoints or NAT
        ──► NAT ──► external vendors (Twilio, FCM/APNs, Slack, MS Graph, WhatsApp BSP, tenant webhooks via egress proxy)
   insights (Python): EventBridge Scheduler ──► ECS RunTask (Spot) ──► read replica/read-only role ──► rollups
 Shared: ECR, S3, KMS, SSM/Secrets Manager, CloudWatch, OTLP collector sidecar
```
Notes:
- **gRPC** works through ALB with an HTTP/2 target group on a dedicated host (`grpc.<domain>`).
- **Service-to-service** traffic uses ECS Service Connect.
- **Tasks run in private subnets**; only the ALB is public. RDS is never publicly reachable.
- Security groups chain: ALB → api/web/mcp; api/dispatcher/scheduler/insights → pgbouncer/RDS.
- IAM: `sqs:SendMessage*` only for api/scheduler roles; `sqs:ReceiveMessage/DeleteMessage/ChangeMessageVisibility/SendMessage` for dispatcher on its channel queues; `sns:Publish` (phone numbers only, no topics) and `ses:SendEmail` only for the dispatcher; `kms:Decrypt` on the tenant-data key only for api/dispatcher/scheduler.

## 4. Networking and egress
- VPC with **2 AZs from day one** (CIDR plan reserves a 3rd), public subnets (ALB, NAT), private app subnets, isolated DB subnets.
- **Free gateway endpoint** for S3. Interface endpoints for **SQS** (high request volume), then SNS, SES, ECR, CloudWatch Logs, KMS once NAT data-processing exceeds their cost (≈ $0.01/hr/AZ + $0.01/GB each; verify Mumbai pricing).
- Stage A: `fck-nat` with a fixed Elastic IP. Single point of failure: acceptable for alpha only.
- Before production: NAT Gateway per AZ (or AWS regional NAT if available and suitable). NAT Gateway is $0.045/hr plus $0.045/GB processed, plus $0.005/hr per public IPv4 (us-east-1 list).
- Fixed egress IPs (EIPs on NAT) are a selling point for tenant webhook allowlists; document them.
- Every public IPv4 costs $0.005/hr; count them (ALB one per AZ, NAT one each).
- Webhook channel egress goes through a dedicated proxy with deny rules for private ranges and the metadata service.

## 5. Data tier
- **RDS PostgreSQL 16/17**, gp3, KMS encryption, automated backups with PITR (7–14 days), deletion protection, tuned parameters (`max_connections`, `work_mem`, aggressive autovacuum on `dispatch_outbox` and current `notifications` partitions).
- Sizing: A = `db.t4g.small` single-AZ; B = `db.t4g.medium`, single-AZ until the **pre-prod Multi-AZ flip** (doubles instance cost); C = `m6g/r6g.large` Multi-AZ + read replica. Watch T-class CPU credits.
- **Multi-AZ flip** (ADR-008): `multi_az = true` is an in-place modification; schedule it in a maintenance window, then run the forced-failover drill under load (docs/07).
- **Connection pooling:** PgBouncer (transaction mode) from Stage B. `SKIP LOCKED` (outbox relay) works in transaction mode; advisory locks and LISTEN/NOTIFY don't, so the scheduler leader uses a lease row.
- **Partitioning and retention (ADR-006):** `notifications` and `notification_events` partitioned **daily** from day one; the scheduler pre-creates partitions and drops those older than ≈ 100 days (90-day metadata + 7-day horizon + margin). An hourly purge job nulls payload/PII columns per tenant retention (1–7 days). Storage sizing is driven by 90 days of metadata.
- **Encryption:** RDS storage encryption (CMK) plus application-level per-tenant DEKs wrapped by the `tenant-data` CMK. KMS calls are bounded by the in-process DEK cache; budget KMS requests accordingly.
- Read-heavy observability queries → read replica at Stage C.
- Backups: PITR + daily snapshots; Stage C adds cross-region copy (AWS Backup). Quarterly restore drills.

## 6. Queueing (ADR-004)
- Terraform module `sqs-dispatch` creates per-channel queues `notifex-{env}-dispatch-{channel}` and DLQs (`maxReceiveCount` ≈ 5), SSE-KMS, retention 4 days (messages are re-creatable from Postgres anyway), visibility timeout > p99 vendor latency (start 60 s, extended by heartbeat).
- Producers set `MessageGroupId = tenant_id` (fair queues). No payload in messages.
- Metrics/alarms per queue: `ApproximateAgeOfOldestMessage`, `ApproximateNumberOfMessagesVisible`, DLQ `ApproximateNumberOfMessagesVisible > 0`, plus app metric **outbox lag**.
- Cost: SQS standard is billed per request (≈ $0.40 per million after the 1M/month free tier in us-east-1; verify Mumbai). Expect 3–5 requests per notification; use batching (send/receive/delete in 10s).

## 7. Platform messaging (ADR-005)
- **SNS SMS:** `Publish` to phone numbers, `AWS.SNS.SMS.SMSType=Transactional`. India traffic uses international routes (no DLT in v1): numeric sender, higher price. Enable SNS SMS delivery-status logging to CloudWatch Logs (sampled success, all failures) and ingest into the timeline.
- Account-level SMS spend quota and an enforced monthly limit; Notifex's **platform-wide daily SMS cap** is set below it so the account quota is never hit (hitting it stops SMS for everyone). Alarms at 70%/90% of spend.
- **SES:** verified shared sending domain (DKIM, SPF, DMARC), configuration set with event destination (SNS/EventBridge) for delivery/bounce/complaint, per-tenant bounce/complaint tracking feeding auto-suspension. Tenant custom domains verified on Notifex SES.
- Vendor fees are excluded from the infra cost model and passed through in pricing (ADR-007).

## 8. Compute and deployment
- One **Go image** (distroless/static, ARM64) with entrypoints: `api`, `dispatcher`, `scheduler`, `mcp`, `migrate`. One **Python image** (`insights`). One **Next.js image** (`output: 'standalone'`).
- ECS services: `api` (on-demand), `web` (on-demand), `mcp` (on-demand), `dispatcher` (one service per channel group or one service with per-channel pools; base on-demand, burst Spot), `scheduler` (1 task Stage A, 2 tasks with lease-based leader before prod), `pgbouncer`.
- All services spread across both AZs (`placement` via subnets); ≥ 2 tasks per service before production.
- Graceful shutdown (handle `SIGTERM`; Spot gives a 2-minute warning): stop receiving, finish or release in-flight messages (visibility → 0) and leases.
- Autoscaling: `api` on CPU + ALB request count; `dispatcher` on **SQS queue age and visible messages per channel**; `web`/`mcp` on CPU/requests.
- Deployments: rolling with circuit breaker + auto-rollback; migrations as a one-off ECS task before rollout, expand/contract only; envelope `v` bumps follow a two-step rollout. Blue/green from Stage C.
- CI/CD (GitHub Actions): lint/test/scan → ARM images → ECR → Terraform plan on PR → apply on merge (manual approval for prod) → ECS deploy → smoke + synthetic checks → auto-rollback.

## 9. Frontend hosting (Next.js)
Default: Fargate container (0.25–0.5 vCPU) behind CloudFront; static assets cached at the edge. Cheaper-at-idle alternatives later: OpenNext on Lambda or Amplify Hosting.
Rules: no secrets in client bundles; server components call the API server-side; same-site cookies; CSP and security headers; image optimization limited or off.

## 10. Security on AWS (baseline from day one)
- IAM: one task role per service (least privilege, scoped to specific queues/keys), separate execution role; permission boundaries for CI roles.
- KMS CMKs: RDS, S3, SQS, logs, `tenant-data` (DEK wrapping for payloads and BYO credentials). Key policies reviewed; key deletion protected by a waiting period.
- WAF managed rule groups + rate-based rules; app-level per-tenant limits remain primary.
- CloudTrail org trail to a locked S3 bucket, GuardDuty, S3 Block Public Access, ECR scanning; Security Hub/Config at Stage C.
- Webhook-channel SSRF defense via egress proxy; IMDSv2 required.
- No payload/PII in SQS messages, logs or traces.

## 11. Observability and cost of observability
- Structured JSON logs at `info`, no payloads/PII, **retention 14 days prod / 7 days staging**; retention set on every log group in Terraform (incl. SNS delivery-status log group).
- OpenTelemetry in Go and Python → OTLP → collector sidecar → backend; traceparent carried in the SQS envelope.
- Custom metrics: accept latency, outbox lag, queue age per channel, DLQ depth, claim duplicates, dispatch success/fail by channel, vendor latency/errors, daily SMS usage vs platform cap.
- Alarms (SNS → Slack/PagerDuty): accept p99, 5xx rate, outbox lag, queue age, DLQ > 0, RDS CPU/connections/storage/replica lag, Spot interruption rate, NAT bytes, **SMS spend vs quota**, SES bounce/complaint rate, **estimated charges**.
- Dogfood: Notifex sends its own alerts through Notifex (with a direct SNS fallback for platform-down alerts).

## 12. Cost model (order-of-magnitude, monthly)
**Method.** us-east-1 list prices verified in 2026: Fargate ARM $0.03238/vCPU-hr and $0.00356/GB-hr; Spot up to ~70% cheaper; RDS db.t4g.small $0.032/hr, db.t4g.medium $0.065/hr (Multi-AZ doubles); NAT Gateway $0.045/hr + $0.045/GB; public IPv4 $0.005/hr; CloudWatch Logs $0.50/GB; SQS ≈ $0.40/M requests. **Mumbai is roughly 10–30% higher.** Vendor fees (SMS via SNS, email via SES, WhatsApp) are excluded and passed through. **Validate every figure with the AWS Pricing Calculator for ap-south-1.**

| Line item | A — Alpha | B — Beta (staging + prod, pre-flip) | B′ — Prod launch (Multi-AZ flipped) | C — GA |
|---|---|---|---|---|
| Fargate (api, web, mcp, dispatcher, scheduler, pgbouncer) | ≈ $30–40 | ≈ $120–140 | ≈ $150–180 | ≈ $250–350 |
| RDS PostgreSQL | t4g.small single-AZ + 50 GB ≈ $30 | t4g.medium single-AZ + 100 GB ≈ $60–65 | t4g.medium Multi-AZ + 150 GB ≈ $125–135 | m6g/r6g.large Multi-AZ + replica + 300 GB ≈ $380–480 |
| SQS (requests) | ≈ $0 (free tier) | ≈ $1–5 | ≈ $2–10 | ≈ $10–40 |
| ALB | ≈ $25–30 | ≈ $35–45 | ≈ $35–45 | ≈ $55–90 |
| NAT / egress / endpoints | fck-nat ≈ $7 | 1 NAT GW ≈ $40 | 2 NAT GW + SQS endpoint ≈ $100 | ≈ $150 |
| WAF, CloudFront, Route 53 | ≈ $12–18 | ≈ $20–30 | ≈ $20–30 | ≈ $35–60 |
| KMS, Secrets, SSM, ECR, S3 | ≈ $8–12 | ≈ $15–25 | ≈ $15–25 | ≈ $30–60 |
| Logs, metrics, alarms, tracing | ≈ $5–10 | ≈ $25–60 | ≈ $30–70 | ≈ $150–300 |
| Staging environment | (shared) | ≈ $70–90 | ≈ $70–90 | ≈ $120–200 |
| **Approx. total (us-east-1 list)** | **≈ $120–160** | **≈ $390–500** | **≈ $550–700** | **≈ $1,200–2,700** |
| Mumbai adjustment | ≈ $130–210 | ≈ $430–650 | ≈ $600–910 | ≈ $1,300–3,500 |

Unit economics: track **infra cost per million delivered notifications** (excluding vendor fees) monthly; target a falling curve.

### Biggest cost levers (apply in this order)
1. **Graviton (ARM) everywhere.**
2. **Spot for dispatcher/batch.**
3. **Right-size** tasks monthly from metrics.
4. **Kill idle**: staging off-hours, ECR/S3 lifecycle rules, unused EIPs.
5. **Trim logs**: retention, sampling, no payload logging; sample SNS success logs.
6. **Egress discipline**: SQS/SES/SNS VPC endpoints when NAT bytes justify them; same-AZ traffic; fewer public IPv4s.
7. **SQS batching** (10 per call) for send/receive/delete.
8. **Commitments after stability**: Compute Savings Plans and RDS RIs after 2–3 months steady usage.
9. **Credits**: AWS Activate/startup credits (verify current terms).

### Cost governance (set up in Stage A)
AWS Budgets with forecast alerts (50/80/100%), Cost Anomaly Detection, cost-allocation tags (`env`, `service`, `owner`) via Terraform default tags, "estimated charges" alarm, **SMS spend alarm**. Monthly 30-minute cost review.

## 13. Stages, exit signals, and scale triggers
| Stage | Shape | Move to next when |
|---|---|---|
| **A — Alpha** | 1 env, 2-AZ network, 1 task per service, single-AZ RDS, fck-nat, SQS + DLQs, SES/SNS in sandbox | real customer data planned, or first design partner |
| **B — Beta** | prod + staging accounts, PgBouncer, NAT Gateway, WAF, alarms, backups tested, SMS/SES production access | ready for production launch |
| **B′ — Prod launch gate** | **RDS Multi-AZ, NAT per AZ, ≥ 2 tasks per service, forced-failover drill passed** (ADR-008), GA gates (ENGINEERING_CHARTER §5) | paying customers |
| **C — GA** | bigger DB + read replica, autoscaling tuned, Savings Plans, cross-region backups, blue/green, SOC 2 controls | any trigger below |
| **D — Scale** | partition/shard strategy, cell-based tenants, optional DR region, Kafka only if a new requirement demands it | revenue-driven |

**Scale triggers (calibrate with doc 07):**
- DB CPU > 60% sustained or accept tx p95 rising → tune batch inserts/indexes → larger instance → PgBouncer tuning → read replica for dashboards → partition by tenant/time.
- Outbox lag p95 > 1 s with DB headroom → more relay workers (sharded by id range) or larger batches.
- Queue age > SLO with CPU headroom → add dispatcher tasks for that channel (Spot); check vendor rate limits first.
- Accept p99 > 100 ms → add API tasks, check pool and tx time, DEK cache hit rate.
- Noisy-neighbor complaints despite fair queues → tighter per-tenant consumer concurrency caps → dedicated queues for top tenants → cells.
- Data residency or DR requirements → second region (warm standby), then active/active only if needed.

## 14. Terraform layout (to be built by Claude Code)
```
deploy/terraform/
  bootstrap/        # org baseline, state bucket (S3, native locking), GitHub OIDC roles, budgets
  modules/
    network/        # vpc (2 AZ, 3rd reserved), subnets, endpoints (S3 gw; SQS/SNS/SES/ECR/Logs/KMS interface toggles), fck-nat | nat-gateway-per-az switch
    ecs-cluster/    # cluster, capacity providers (FARGATE, FARGATE_SPOT), Service Connect namespace
    ecs-service/    # task def, service, AZ spread, autoscaling (CPU / SQS age), logs (retention), IAM, alarms
    scheduled-task/ # EventBridge Scheduler -> ECS RunTask (insights, migrations)
    rds-postgres/   # instance, params, subnet group, backups, alarms, KMS, multi_az toggle
    pgbouncer/      # small ECS service
    sqs-dispatch/   # per-channel queues + DLQs (for_each), SSE-KMS, redrive, alarms
    messaging/      # SES domain/DKIM/config set/event destinations; SNS SMS prefs, delivery-status logging, spend alarms
    alb/            # listeners, host/path rules, gRPC target group, WAF association
    edge/           # CloudFront, ACM, Route53, WAF
    ecr/            # repos + lifecycle policies + scanning
    secrets-kms/    # CMKs (rds, s3, sqs, logs, tenant-data), SSM/Secrets
    observability/  # alarms, dashboards, SNS alert topic, OTLP collector config
    budgets/        # budgets, anomaly detection
  envs/{staging,prod}/   # compose modules; tfvars hold sizes per stage
```
Standards: pinned provider versions, `terraform fmt/validate`, tflint, tfsec/checkov in CI, default tags, no hardcoded account IDs, `var.stage = "alpha|beta|prod|ga"` drives `rds.multi_az`, `nat.mode`, `ecs.min_tasks`, endpoints; drift detection nightly.

## 15. Prompts for Claude Code (infrastructure track)
- **I0:** "Read CLAUDE.md, ENGINEERING_CHARTER.md, docs/10 and docs/adr/. Plan the Terraform bootstrap (org baseline, state, OIDC, budgets) for ap-south-1. List resources, IAM, and estimated monthly cost. Wait for approval."
- **I1:** "Build network (2 AZ), ecs-cluster, rds-postgres (multi_az toggle off), sqs-dispatch, secrets-kms, ecr, alb and ecs-service modules for Stage A with ARM64 Fargate, fck-nat, gRPC target group, log retention, default tags, and alarms. Include validate, tflint and tfsec in CI. Show the plan output and cost estimate."
- **I2:** "Write the GitHub Actions pipeline: build ARM64 images, scan, push to ECR, run migrations as a one-off ECS task, deploy with circuit breaker and auto-rollback, run smoke tests. OIDC only."
- **I3 (Stage B):** "Add staging/prod split, PgBouncer, NAT Gateway, WAF, messaging module (SES domain + config set, SNS SMS delivery logging + spend alarms), Spot capacity for dispatcher with graceful SIGTERM, and SQS-age autoscaling per channel."
- **I3′ (prod gate):** "Flip stage to prod: RDS Multi-AZ, NAT per AZ, ≥ 2 tasks per service, SQS VPC endpoint. Produce the failover drill runbook and k6 scenario per docs/07."
- **I4 (Stage C):** "Add read replica, cross-region backups, blue/green deploys, Savings Plan analysis, DR runbook and a restore-drill script."

## 16. Known limits and honest caveats
- Terraform cannot be run or applied from this environment; all IaC must be validated and applied by you with plans reviewed.
- Cost figures are estimates from list prices; real bills depend on traffic mix, logs, NAT bytes, SQS request counts and region. Re-check against the Pricing Calculator and Cost Explorer after the first month.
- Stage A/B single-NAT and single-AZ RDS trade availability for cost; do not take paying production traffic before the B′ gate.
- SMS to India over international routes (no DLT) can have higher cost and delivery variance; measure in beta.
