# ADR-007 — Pricing unit: per delivered notification

- **Status:** Accepted (2026-11-27)
- **Decider:** Shobhit

## Decision
Tenants are charged **per notification delivered**. One notification = one action × one recipient. Failed, denied (policy), cancelled and expired notifications are **not** billed. Retries of the same notification are billed **once**.

**Exception (decided):** platform-managed channels where Notifex pays the vendor per attempt (**platform SMS**, and platform WhatsApp if it is added later) bill on **`sent`**, meaning the vendor accepted the message. This protects margin, because we pay the vendor whether or not a delivery report arrives.

## Billable signal per channel
| Channel | BYO / no per-attempt vendor cost | Platform SMS (/WhatsApp later) |
|---|---|---|
| Email | SES/SendGrid `Delivery` event | — (platform email bills like BYO; SES cost is negligible) |
| SMS | Vendor delivery report = delivered | **`sent`** (vendor accepted) |
| WhatsApp | `delivered` status callback | **`sent`** |
| Push | FCM/APNs accepted (no device receipt exists) | n/a |
| Slack / Teams / Outlook | API success | same |
| Webhook | Tenant endpoint returned 2xx | n/a |

**Missing receipts on delivery-billed async channels:** if a notification is `sent` with no receipt after **72 h**, mark it `sent_unconfirmed` and do not bill.

## Pricing shape
- Platform SMS/WhatsApp: pass-through vendor cost + margin per message, by destination country.
- BYO channels: Notifex platform fee per delivered notification only.
- Plans: free monthly allowance, then per-unit by channel class (cheap: email/push/chat/webhook; metered: SMS/WhatsApp). Pricing pages must say clearly that platform SMS bills on send.

## Metering design
- `usage_records` (`tenant_id`, `notification_id` **UNIQUE**, `channel`, `credential_mode`, `billing_basis: delivered|sent`, `country`, `billable_at`, `unit_price_ref`). Written in the **same tx** as the status transition to the billable state; the unique key makes at-least-once duplicates and late receipts idempotent.
- `billing_basis` is decided at accept time from the resolved credential mode (in the snapshot), so a credential change mid-flight never changes how a notification is billed.
- Hourly rollups → `usage_daily` → billing provider (Stripe or Razorpay for INR/GST). Not PII, so retained beyond the 7-day payload window.
- Console shows live usage vs. allowance; quota enforcement at accept time uses accepted counts as a leading indicator.

## Consequences / follow-ups
- Delivery-receipt callbacks are billing-critical: signed, idempotent, replay-safe (docs/06).
- docs/08 launch plan and docs/09 pricing comparison to be updated.
