# 03 — Domain Model and API Contracts

> Updated 2026-11-27 for ADR-004 to ADR-008.

## Entities (ent schemas — start with these, refine as needed)
All tenant-owned entities carry `tenant_id`, `created_at`, `updated_at`. 🔒 = envelope-encrypted column (per-tenant DEK, ADR-006). ⏳ = purged at tenant payload retention.

| Entity | Key fields |
|---|---|
| `Tenant` | name, slug, status, plan, quotas (JSON), `platform_sms_daily_limit`, `sandbox` (bool), `dek_wrapped`, `dek_key_id` |
| `User` | email, password_hash?, oauth identities, status |
| `Membership` | tenant_id, user_id, role (`owner/admin/developer/viewer`) |
| `BrandProfile` | tenant_id, application_id?, logo_url, colors (JSON), sender identities per channel, footer, legal_text, default_locale |
| `Policy` | tenant_id, application_id?, quiet_hours, timezone, rate_limits, allowed_channels, max_attempts, `payload_retention_days` (1–7, default 7), pii_masking |
| `Application` | tenant_id, name, key (slug), description |
| `Environment` | application_id, name (`dev/staging/prod`) |
| `ApiKey` | application_id, environment, prefix, hash, scopes, last_used_at, revoked_at |
| `ChannelCredential` | tenant_id, application_id?, environment?, channel, provider, `mode` (`platform/byo`), `secret_ciphertext` 🔒 (BYO only), status (`pending/verified/disabled`), verified_at |
| `Workflow` | application_id, key (stable slug), name, payload_schema (JSON schema, optional), status (`draft/active/archived`), exposure flags (MCP/Terraform, doc 12) |
| `Action` | workflow_id, channel, order/config, enabled, conditions (JSON) |
| `Template` | action_id, channel, locale, subject?, body, extra (bcc, cc, buttons, blocks…) |
| `Run` | tenant_id, application_id, environment, workflow_key, snapshot (JSONB: workflow, templates, brand, policy, credential refs), `payload` 🔒⏳, context (JSONB), idempotency_key, source, actor, status, schedule_at |
| `Notification` | tenant_id, run_id, channel, `recipient` 🔒⏳, recipient_masked, recipient_hash (keyed HMAC), status, attempt, `lease_until`, last_error_code, last_error (redacted), vendor_message_id, `credential_id`, `credential_mode`, `billing_basis` (`delivered/sent`), `rendered_content` 🔒⏳, `vendor_response` 🔒⏳, terminal_at, purged_at. **Partitioned daily on created_at.** |
| `NotificationEvent` | tenant_id, notification_id, type, at, detail (redacted) — the timeline. Partitioned daily. |
| `DispatchOutbox` | id (bigserial), tenant_id, notification_id, channel, delay_s (≤ 900), attempt, created_at, published_at |
| `ScheduledDispatch` | tenant_id, notification_id, due_at, reason (`scheduled/deferred/backoff`), attempt |
| `PlatformSmsUsage` | tenant_id, day (UTC date), count — PK (tenant_id, day) |
| `IdempotencyRecord` | tenant_id, key, request_hash, run_id, expires_at |
| `AuditEvent` | tenant_id?, actor, action, entity, before/after (redacted), at |
| `UsageRecord` | tenant_id, `notification_id` (UNIQUE), channel, credential_mode, billing_basis, country, billable_at, unit_price_ref |
| `UsageDaily` | tenant_id, day, channel, credential_mode, country, count |

Removed: `DispatchJob` (replaced by SQS + `DispatchOutbox` + `ScheduledDispatch`), `VendorCredential` (renamed `ChannelCredential`).
No `workflow_versions` or `template_versions`. Snapshots on `Run` provide historical fidelity.

## Statuses
- Run: `accepted → processing → completed | partially_failed | failed | canceled`
- Notification: `queued → deferred? → sending → retrying? → sent → delivered`, terminal alternatives `sent_unconfirmed | failed | suppressed | canceled | expired`.
  - `sent_unconfirmed`: async channel with no receipt 72 h after `sent` (not billed unless billing basis is `sent`).
  - `suppressed`: policy deny. `expired`: exceeded 48 h retry/deferral window.
- Billable when: `billing_basis = delivered` and status reaches `delivered`; or `billing_basis = sent` (platform SMS) and status reaches `sent`.

## Retention (ADR-006)
| Data | Retention |
|---|---|
| 🔒⏳ columns (payload, recipient, rendered content, vendor response) | `payload_retention_days` (1–7) after `terminal_at`, then set NULL, `purged_at` set |
| Notification/Run/Event metadata | 90 days (partition drop at ≈ 100 days) |
| UsageRecord / UsageDaily | statutory |
| AuditEvent | per compliance plan (separate from payload retention) |

## Roles and permissions (tenant console)
| Capability | viewer | developer | admin | owner |
|---|---|---|---|---|
| View apps/workflows/runs/metrics/usage | ✓ | ✓ | ✓ | ✓ |
| Edit workflows/templates | | ✓ | ✓ | ✓ |
| Create/revoke API keys | | ✓ (non-prod) | ✓ | ✓ |
| View channel credential metadata (never secrets) | | ✓ | ✓ | ✓ |
| Manage channel credentials, brand, policy (incl. retention) | | | ✓ | ✓ |
| Replay/cancel notifications | | ✓ | ✓ | ✓ |
| Manage members and roles | | | ✓ (not owners) | ✓ |
| Billing, plan, delete tenant (crypto-shred), transfer ownership | | | | ✓ |

**Platform operator** is a separate realm (not a tenant role): tenant suspension, sandbox lift, plan limits (incl. `platform_sms_daily_limit`), platform-wide SMS cap, global health, with audited access.

## Trigger API (REST)
`POST /v1/trigger` — header `Authorization: Bearer <api_key>`, header `Idempotency-Key: <uuid>`
```json
{
  "workflow": "order_shipped",
  "recipients": [
    { "id": "r1", "email": "a@example.com", "phone": "+919999999999",
      "push_tokens": ["..."], "slack_user": "U123", "whatsapp": "+919999999999",
      "locale": "en", "timezone": "Asia/Kolkata" }
  ],
  "payload": { "order_id": "A123", "tracking_url": "https://..." },
  "context": { "source": "checkout-service", "trace_id": "..." },
  "channels": ["email", "sms"],
  "schedule_at": "2026-12-01T09:00:00Z"
}
```
Response `202`:
```json
{ "run_id": "run_...", "status": "accepted",
  "notifications": [
    { "id": "ntf_...", "channel": "email", "recipient_ref": "r1", "status": "queued" },
    { "id": "ntf_...", "channel": "sms", "recipient_ref": "r1", "status": "failed",
      "error": { "code": "platform_sms_daily_limit", "retry_after": "2026-11-28T00:00:00Z" } }
  ] }
```
- `channels` (optional) narrows the workflow's configured actions; it can never add channels the workflow lacks.
- `schedule_at` must be ≤ now + 7 days.
- Per-action rejections (credential missing, daily SMS limit, channel disallowed) are reported **per notification** in a `202`; the request fails as a whole only if *no* notification can be accepted.

### Error codes
| HTTP | code | Meaning |
|---|---|---|
| 400 | `validation_failed` | payload/recipient/schema invalid |
| 400 | `schedule_horizon_exceeded` | `schedule_at` > now + 7 days |
| 401 | `unauthenticated` | bad/missing API key |
| 403 | `channel_not_allowed` | policy disallows channel |
| 404 | `workflow_not_found` | |
| 409 | `idempotency_conflict` | same key, different request hash |
| 429 | `rate_limited` / `quota_exceeded` | with `Retry-After` |
| per-notification | `credential_unavailable` | no BYO and platform not allowed |
| per-notification (429 if all) | `platform_sms_daily_limit` | tenant's daily platform SMS limit reached |
| 503 | `unavailable` | e.g. DB failover; retry with same idempotency key |

## Other REST endpoints (v1)
- `GET /v1/runs/{id}`, `GET /v1/notifications/{id}` (timeline; payload fields shown only if not purged and caller has rights), `GET /v1/runs?workflow=&status=&from=&to=`
- `POST /v1/notifications/{id}/replay` (409 `payload_expired` if purged), `POST /v1/runs/{id}/cancel`
- Control plane under `/v1/admin/...`: tenants, members, applications, environments, api-keys, workflows, actions, templates, brand, policies, **channel-credentials** (create/rotate/verify/disable; secrets write-only), audit-events, metrics, **usage** (daily usage, daily SMS remaining).
- `POST /v1/templates/preview` — render a template with sample payload + brand (SMS shows segment count).
- Vendor callbacks: `POST /callbacks/{provider}` (signature verified, idempotent).
- OpenAPI 3.1 spec is the source of truth; generate docs and SDK scaffolding from it.

## gRPC (`api/proto/notifex/v1/`)
```proto
service NotifexService {
  rpc Trigger(TriggerRequest) returns (TriggerResponse);
  rpc TriggerBatch(TriggerBatchRequest) returns (TriggerBatchResponse);
  rpc GetRun(GetRunRequest) returns (Run);
  rpc GetNotification(GetNotificationRequest) returns (Notification);
  rpc StreamNotificationEvents(StreamRequest) returns (stream NotificationEvent);
}
```
- Auth via `authorization` metadata (API key). Idempotency key in request field. Error codes map to `google.rpc.ErrorInfo.reason` with the same strings as REST.
- Use `buf` for lint/breaking-change checks. Same service layer as REST; transports stay thin.

## Queue envelope (internal contract, ADR-004)
```json
{"v":1,"tenant_id":"…","run_id":"…","notification_id":"…","channel":"sms",
 "attempt":1,"enqueued_at":"…","traceparent":"…"}
```
SQS: `MessageGroupId = tenant_id`. Never add payload or recipient fields. Bump `v` for incompatible changes; consumers must support current and previous version during rollout.

## SDK contract
Go, Node/TypeScript, Python. Thin wrappers over the generated clients:
```go
client := notifex.New(notifex.WithAPIKey(key))
run, err := client.Trigger(ctx, notifex.TriggerInput{
    Workflow:   "order_shipped",
    Recipients: []notifex.Recipient{{Email: "a@example.com"}},
    Payload:    map[string]any{"order_id": "A123"},
    IdempotencyKey: id,
})
```
Requirements: automatic idempotency key generation if absent (reused across retries), retries with jittered backoff on `429/503/5xx`, typed errors (including per-notification errors), context/timeout support, optional gRPC transport, sandbox mode (`mock` vendors).

## Template language
- Handlebars-style (`{{payload.order_id}}`, `{{#if}}`, `{{#each}}`, `{{brand.primary_color}}`, `{{recipient.locale}}`).
- Rendering is sandboxed: no function calls, no I/O, execution time and output size limits.
- HTML email is sanitized; plain-text part auto-generated.
- Per-channel limits validated (SMS ≤ 3 segments, push payload size, Slack block schema, WhatsApp approved-template variables).

## Terraform provider resources
`notifex_application`, `notifex_workflow`, `notifex_action`, `notifex_template`, `notifex_brand_profile`, `notifex_policy`, `notifex_api_key`, data sources for workflows/applications. Credential secrets are **not** managed via Terraform (portal only).
