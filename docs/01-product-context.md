# 01 — Product Context

## Vision
Notification infrastructure as a service. Product teams should integrate once, configure everything in a console (or Terraform), and never think about vendor APIs, retries, rate limits, scaling, or delivery tracking again.

## Personas
| Persona | Needs |
|---|---|
| **Backend developer** (tenant) | Simple SDK/API/gRPC call, idempotency, clear errors, sandbox, delivery status |
| **Product/marketing owner** (tenant) | Edit templates and workflows without a deploy; brand-consistent output |
| **Ops/SRE** (tenant) | Per-workflow and per-notification monitoring, failure alerts, retry controls |
| **Compliance/admin** (tenant) | Policies (quiet hours, channel allowlist, retention), audit trail |
| **Platform operator** (Notifex staff) | Tenant health, quotas, noisy-neighbor control, vendor status |

## Core concepts (glossary)
- **Tenant:** a customer organization. Isolation boundary for data, quotas, brand, policy.
- **Application:** a product/service owned by a tenant (e.g. "Checkout API"). Has environments (`dev`, `staging`, `prod`) and API keys.
- **Workflow:** a named notification definition (e.g. `order_shipped`). Contains one or more **actions**.
- **Action:** one channel delivery inside a workflow (email, sms, push, slack, outlook, whatsapp, webhook). Actions run independently and in parallel.
- **Template:** channel-specific content for an action (subject, body, bcc, buttons, etc.). Workflow-scoped. Rendered with the trigger payload.
- **Trigger:** an API/gRPC/SDK call: `workflow_key + payload + recipients (+ context, idempotency_key, schedule)`.
- **Run:** one accepted trigger. A run has N **notifications**, one per action/recipient.
- **Notification:** one delivery attempt chain to one recipient on one channel. Has a status timeline.
- **Brand profile:** per-tenant (optionally per-application) logo, colors, sender names/addresses, footer, legal text, locale defaults.
- **Policy:** per-tenant rules applied at dispatch time (quiet hours + timezone, rate limits, channel allowlist, retry limits, data retention, PII masking).

## In scope (v1)
1. Tenant registration, org/members, four roles, OAuth login.
2. Applications, environments, API key management.
3. Workflow + template builder (console + API + Terraform).
4. Trigger via REST, gRPC, and SDKs (Go, Node, Python).
5. Channels: email, SMS, push (FCM/APNs), Slack, Outlook (Microsoft Graph mail) / Teams, WhatsApp (Business API), webhook.
6. Template rendering with variables, conditionals, and brand injection.
7. Reliable delivery: queue, retries with backoff, at-least-once, idempotency.
8. Monitoring: run timeline, per-notification status, per-workflow and per-channel dashboards, failure drill-down, replay.
9. Multi-tenancy with brand and policy enforcement.
10. Scheduled/delayed triggers.
11. Audit log; usage metering per tenant.

## Explicit non-goals (v1)
- No subscriber/contact/user directory, no preference center (callers pass recipients inline).
- No campaign/marketing-blast tooling, no segmentation.
- No template/workflow version history tables.
- No in-app notification inbox/feed.
- No building our own SMTP/SMS carrier; we integrate vendors.

## Non-functional targets
- Trigger accept latency: p99 < 100 ms (accept = validated, snapshotted, enqueued).
- Throughput: design for 5k triggers/sec per region on the accept path; dispatch scales horizontally with workers.
- Delivery: at-least-once; duplicates minimized via per-vendor idempotency where supported.
- Availability: 99.9% accept path.
- Zero cross-tenant data exposure.
- Observability data (timeline) queryable within 5 s of event.
- Not production-ready yet; treat hardening as a tracked workstream (see roadmap).

## Business notes
- SaaS, usage-based pricing (per accepted notification, per channel), tenant quotas enforced by the platform.
- Candidate for AWS Marketplace listing later: keep deployment reproducible (Terraform) and metering clean.
