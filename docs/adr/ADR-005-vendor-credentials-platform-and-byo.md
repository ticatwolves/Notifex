# ADR-005 — Vendor credentials: platform-managed and bring-your-own (BYO)

- **Status:** Accepted (2026-11-27; India SMS approach revised same day)
- **Decider:** Shobhit

## Decision
Support both modes per channel:
- **Platform-managed:** Notifex's own vendor accounts. Zero setup, aimed at new teams and MCP/AI-tool users who don't have credentials.
- **BYO:** the tenant's own vendor account. Their sender identity, reputation and vendor bill.

## Credential resolution
Table `channel_credentials` (`tenant_id`, `application_id NULL`, `environment NULL`, `channel`, `provider`, `mode: platform|byo`, `secret_ciphertext` (BYO only, encrypted under the tenant DEK), `status`, `verified_at`).

Resolution order at **trigger acceptance**: environment BYO → application BYO → tenant BYO → platform-managed (only if the tenant's plan and the channel allow it). Otherwise the action fails with `credential_unavailable` and no send is attempted.

- The **resolved credential reference** (`credential_id`, `mode`, `provider`) is written into the run snapshot, consistent with the snapshot-at-accept rule.
- The **secret** is fetched and decrypted at dispatch time, so rotations take effect for in-flight runs. If the referenced credential has been deleted, the notification fails permanently.
- BYO secrets use envelope encryption with the **per-tenant DEK** (wrapped by a KMS CMK, ADR-006). Platform credentials are the dispatcher's **IAM task role** (SNS/SES); there is no stored secret.

## Launch matrix
| Channel | Platform-managed | BYO | Notes |
|---|---|---|---|
| Email | ✅ Notifex SES, shared sending domain; optional tenant domain verification (DKIM) still on Notifex SES | ✅ SES / SendGrid / SMTP | Shared-domain reputation is a platform-wide risk |
| SMS | ✅ **Amazon SNS** (Notifex account), **per-tenant daily limit**, no DLT in v1 | ✅ Twilio / MSG91 etc. | See "Platform SMS via SNS" |
| WhatsApp | ❌ at launch | ✅ tenant WABA / BSP | Later: Meta Tech Provider embedded signup |
| Push (FCM/APNs) | ❌ impossible: keys belong to the tenant's mobile app | ✅ only | |
| Slack | ✅ Notifex Slack app, tenant installs via OAuth | ✅ incoming webhook / own bot token | |
| Outlook / Teams | ✅ Notifex multi-tenant Entra app, tenant admin consent | ✅ tenant app registration | |
| Webhook | n/a | n/a | Per-tenant signing secret only |

## Platform SMS via SNS (decided)
- Provider: `VendorClient` impl `sns`, calling SNS `Publish` to a phone number with `SMSType=Transactional`. (SNS SMS is delivered through AWS End User Messaging SMS under the hood.)
- **No DLT registration in v1.** Without a registered entity/template, SNS sends to Indian numbers over **international (ILDO) routes**: the sender appears as a random numeric ID (no brand sender ID), and the per-message price is higher than local routes. Accepted trade-off for v1; documented to tenants.
- **Per-tenant daily limit:** `platform_sms_daily_limit` on the tenant plan (e.g. sandbox 20/day, paid configurable). It counts **messages** (one notification to one number); each message is capped at **3 segments** by `Validate`, so cost per message is bounded.
  - Enforced **at accept time** with an atomic counter: `UPDATE platform_sms_usage SET count = count + $n WHERE tenant_id=$1 AND day=$2 AND count + $n <= $limit` (row upserted per tenant per UTC day). Zero rows updated → the action is rejected with `platform_sms_daily_limit` (other actions in the workflow still proceed; actions are independent).
  - Counter is decremented if a notification fails permanently *before* reaching SNS (e.g. policy deny), so tenants aren't charged quota for messages never sent.
- **Global backstop:** a platform-wide daily cap sized **below** the AWS account's monthly SMS spend quota. AWS stops all SMS sending shortly after the account quota is exceeded, which would hit every tenant at once. Alarm at 70% / 90% of the AWS spend limit.
- **Ops prerequisites:** move the Notifex AWS account out of the SMS sandbox and raise the SMS spend quota (support case, initial reply within ~24 h), per environment account.
- Delivery status: SNS delivery-status logging to CloudWatch Logs → ingested as status events. Platform SMS bills on `sent` (ADR-007), so receipts are observability, not billing-critical.
- **Deferred enhancements:** DLT (Notifex as principal entity, or tenant-supplied entity/template IDs) for local routes and brand sender IDs; third-party SMS providers (e.g. Twilio) as platform options; moving to the End User Messaging SMS v2 API if we need configuration sets and event destinations.

**Risks:** ILDO traffic into India is more expensive and may face higher carrier filtering and delivery variance than DLT local routes; measure delivery rates in beta. Shared-account abuse affects every tenant, hence the daily limits, sandbox and auto-suspension below.

## Guardrails for platform-managed (enforced server-side)
- **Sandbox by default:** new tenants on platform creds get low daily caps (SMS limit above; email daily cap). They lift out of sandbox after verification (email domain, business details) or a paid plan.
- Bounce/complaint/opt-out monitoring per tenant with **automatic suspension** of platform sending when thresholds are crossed.
- Content guardrails on platform email/SMS (link reputation checks, no URL shorteners on shared domains, OTP/transactional use only for platform SMS in v1; no promotional SMS).
- MCP/AI-originated triggers on platform creds inherit the stricter of the MCP guardrails (doc 12) and the sandbox limits.
- Platform-managed usage carries vendor cost and is priced per channel (ADR-007).

## Consequences / follow-ups
- docs/03: add the `ChannelCredential` entity, `platform_sms_daily_limit` on plans, credential endpoints, errors `credential_unavailable` and `platform_sms_daily_limit`, and RBAC (owner/admin manage credentials; developers view metadata, never secrets). *(Done.)*
- docs/06: security tests for credential isolation, decrypt-only-at-dispatch, secret masking, and daily-limit race (concurrent triggers can't exceed the cap).
- docs/10: SNS SMS permissions on dispatcher task role, delivery-status logging, spend alarms. *(Done.)*
- Audit event on every credential create/rotate/delete/verify and every plan-limit change.
