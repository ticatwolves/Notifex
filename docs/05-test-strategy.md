# 05 — Test Strategy and Test Case Catalog

Backend and frontend are separate deployables with separate test suites, joined by contract tests and a full-stack E2E suite.

## Pyramid and tooling
| Layer | Backend (Go) | Frontend (Next.js) | Runs |
|---|---|---|---|
| Unit | `go test -race`, table-driven, `testify` | Vitest + Testing Library | every commit |
| Integration | real Postgres (testcontainers), real queue, `MockVendorClient` | MSW for API mocks | every PR |
| Contract | OpenAPI validation (e.g. schemathesis), `buf breaking`, SDK conformance suite | generated client type-checks | every PR |
| E2E | — | Playwright against full docker-compose stack | every PR (smoke), nightly (full) |
| Security | see doc 06 | see doc 06 | PR (SAST/deps), nightly (DAST) |
| Load | k6 (doc 07) | Lighthouse CI for console | PR (smoke), weekly (full) |
| Resilience | fault injection (toxiproxy), kill/restart workers | offline/slow-network tests | nightly |
| Accessibility | — | axe-core in Playwright | every PR |

Rules: no real vendor calls in any automated test; deterministic clocks (inject `Clock`); every bug fix ships with a regression test; flaky tests are quarantined within 24h and fixed within a week.

## Backend test cases

### Auth and tenancy (100% coverage required)
- Register, login, logout; password policy; lockout/rate limit on repeated failures.
- OAuth happy path and state/nonce mismatch rejected.
- API key: valid, revoked, expired, wrong environment, wrong application, malformed.
- RBAC matrix: table test of every role × every endpoint/capability (expected allow/deny).
- **Tenant isolation:** for every store method and every endpoint, tenant A cannot read, list, update, delete, or trigger against tenant B's resources (IDs guessed, swapped, or nested). Expect 404, not 403, to avoid existence leaks.
- Membership: last owner cannot be removed; admin cannot demote owner; invite token single-use and expiring.
- Audit event written for every control-plane mutation, with actor and redacted diff.

### Workflows, templates, rendering
- CRUD with validation; archived workflows reject triggers.
- Payload schema: missing required, wrong types, extra fields, oversized payload.
- Rendering: variables, nested paths, missing variable behavior (configured: empty vs error), conditionals, loops, locale fallback, brand variable injection (tenant vs application override).
- **Sandbox:** template with infinite loop/huge expansion hits time/size limit; no function calls, file/network access, or prototype/reflection tricks; HTML sanitization strips scripts/handlers; fuzz tests on the renderer.
- Channel limits: SMS segment counting (GSM-7 vs UCS-2), push payload size, Slack block schema, WhatsApp approved-template variable count.
- Plain-text part generated from HTML email.

### Trigger intake
- Valid trigger → `202`, one run, correct notification fan-out (actions × recipients).
- Idempotency: same key + same body → same run, no duplicates; same key + different body → `409`; concurrent identical requests produce exactly one run.
- Snapshot: edit workflow/template after accept; run still uses the snapshot.
- Unknown workflow, disabled action, disallowed channel (policy), `channels` filter can only narrow.
- Quota exceeded and ingest rate limit → `429` with `Retry-After`; other tenants unaffected.
- Scheduled trigger in the past, near future, far future, invalid timezone.
- Batch trigger partial validation behavior defined and tested.
- Atomicity: failure mid-transaction leaves no orphan run/notification/job.

### Dispatch
- Single worker processes job once; N concurrent workers never double-claim (stress test).
- Lease expiry: kill worker mid-send → job requeued and completed exactly once from the platform's perspective (vendor idempotency key reused).
- Retry: retryable errors back off with jitter up to max attempts, then dead-letter; permanent errors fail immediately; classification table-tested per channel.
- Policy: quiet hours defer (not fail) and respect recipient timezone and DST boundaries; channel allowlist denies; per-tenant rate limit smooths bursts.
- Fairness: one tenant enqueues 1M jobs, another's jobs still dispatch within SLO.
- Channel isolation: email failing does not block SMS in the same run; run status becomes `partially_failed` correctly.
- Cancel/replay: cancel prevents pending sends; replay uses the original snapshot and a new attempt chain.
- Poison job (panics, huge payload) does not crash the worker loop.

### Channels (each adapter)
- Validate: bad address formats, missing fields, unsupported content.
- Send: success, 4xx permanent, 429 with `Retry-After`, 5xx, timeout, malformed vendor response, TLS error — against `MockVendorClient` scripted behaviors.
- Credentials: encrypted at rest, decrypted only in the worker, rotated without downtime, never logged.
- Webhook channel: SSRF blocked (private/link-local/metadata IPs, DNS rebinding, redirects), HMAC signature present, timeout and body-size limits.
- Vendor callbacks: signature verification, replay protection, out-of-order status events handled (delivered before sent).

### Observability
- Every state transition emits a timeline event; timeline order is consistent under retries.
- Metrics emitted with bounded label cardinality (no `recipient`/`run_id` labels).
- Trace spans connect intake → job → vendor call.
- PII masking in logs and timeline details verified by test (grep for raw emails/phones in captured logs).

### Data lifecycle
- Retention job deletes payloads and old partitions per policy; never deletes within retention.
- Tenant export and deletion complete and verified; backups restore into a working system (scripted restore test).
- Migrations: apply on empty DB and on a prod-sized snapshot; rollback/expand-contract paths exercised.

## Frontend test cases
- Auth flows (password, OAuth callback errors, session expiry redirects).
- RBAC in UI: controls hidden/disabled per role and, crucially, server still enforces (negative E2E tests hit the API directly).
- Workflow builder: add/remove/reorder actions, validation messages, unsaved-changes guard.
- Template editor: live preview, variable autocomplete, invalid syntax errors, XSS payload in preview is inert (rendered in sandboxed iframe).
- Runs explorer: filters, pagination, empty/loading/error states, large result sets, deep link to a notification timeline.
- API key creation shows secret once; copy works; revoked keys disappear from use.
- Dashboards render with zero data, partial data, and API failure.
- Accessibility (axe) on every core page; keyboard-only operation of builder and tables.
- Responsive layouts at common breakpoints; no layout shift on data load (CLS budget).
- Security headers present (CSP, X-Frame-Options/frame-ancestors, Referrer-Policy); no secrets in client bundles (scan build output).

## SDK conformance suite (run against every SDK)
Shared scenarios: success, idempotent retry, `429` backoff honoring `Retry-After`, `5xx` retry with jitter, timeout/cancellation, typed errors, auth failure, large payload, gRPC and REST parity.

## End-to-end scenarios (Playwright + API, full stack, mock vendors)
1. **Golden path:** register → create app → create workflow with email+SMS+Slack → create API key → trigger via SDK → see all three timelines reach `sent/delivered` in the console.
2. **Partial failure:** configure SMS vendor to fail permanently → run shows `partially_failed`, email delivered, failure drill-down shows reason, replay after fixing succeeds.
3. **Policy:** enable quiet hours → trigger → notification `deferred` with `run_at` shown → advance clock → delivered.
4. **Multi-tenant brand:** two tenants with different brand/policy trigger the same workflow key → each email uses its own brand and obeys its own policy.
5. **Outage and recovery:** stop vendor mock for N minutes → queue builds, no accepts fail, alerts fire → restore → backlog drains, no duplicates.
6. **Idempotent retry storm:** client retries the same trigger 50× concurrently → exactly one run.
7. **Member lifecycle:** invite → accept → role change → permissions change immediately → removal revokes sessions.

## Exit metrics
Test pass rate 100% on main; backend service-layer coverage ≥80%; tenant-isolation and auth ≥100% of paths; zero quarantined tests older than a week; E2E nightly green 14 days before GA.

## Integration surface test cases (see doc 12)
**Parity:** same trigger via REST, gRPC, Connect, MCP tool, Terraform action, CLI and inbound webhook yields equivalent runs, snapshots, audit (`source`/`actor` differ), policy outcomes, and idempotency behavior.
**Credentials and scopes:** every scope x endpoint/tool matrix; allowed_workflows/channels enforced; recipient policy (domain, country code, allowlist, max recipients); caps and rate limits per credential; approval modes (`auto`, `confirm`, `dry_run_only`) behave as configured; kill switch and expiry take effect within seconds; default-deny for unexposed workflows.
**Direct send:** content constraints (length, link domains, no attachments), brand wrapper applied, no profile means denied, dry-run never enqueues.
**MCP:** Inspector conformance; golden transcripts per tool; stateless operation across multiple replicas; spec-revision matrix (2025-11-25 and 2026-07-28); Origin validation; request size limits; OAuth discovery (`WWW-Authenticate` resource_metadata, protected resource metadata), PKCE, Client ID Metadata Document and DCR-fallback registration, audience mismatch rejected, token passthrough absent, scope escalation rejected, revoked token fails immediately; tool errors are actionable and leak nothing cross-tenant; tool annotations match behavior.
**Agent-safety evals (real model + scripted agents):** prompt-injection and over-sharing scenarios (external recipients, bulk lists, instruction-hijack text inside tool output) result in blocked or held-for-approval, never silent sends; repeated agent retries do not duplicate sends.
**Terraform provider:** acceptance tests with terraform-plugin-testing (action invoked via `action_trigger` on create/update and via CLI invoke); plan-time validation (unknown workflow, schema mismatch, disallowed recipient) fails `plan`; nothing is sent at plan; deterministic idempotency across retried applies; `fail_on_error` semantics; behavior on supported Terraform versions; sensitive values never logged.
**Inbound webhooks:** signature verification (HMAC, SNS), replay protection, mapping sandbox (no code execution, size/time limits), malformed/oversized payloads, per-hook rate limits.
**CLI/SDKs:** conformance suite plus stdio MCP mode and device-login flow.
