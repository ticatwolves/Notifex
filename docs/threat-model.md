# Threat Model v1 (STRIDE)

- **Status:** v1, 2026-11-27, written at the end of Phase 0 (Stage 0 exit criterion).
- **Method:** STRIDE per trust boundary, following docs/06 §1. Update on every new surface: Phase 1 (console, OAuth), Phase 3 (trigger and dispatch), Phase 4 (gRPC, vendor callbacks), and the MCP, Terraform and inbound-webhook work (doc 12).
- **How to read the tables:** "Built" names the code or test that enforces a mitigation today. "Planned" names the phase that delivers it. A mitigation without one of those is a gap and appears under §6, Residual risks.

## 1. Scope

| In scope now (Phase 0 code) | Designed but not built (modelled from docs/02, 03, 12) |
|---|---|
| REST `/healthz`, `/readyz`, `/v1/whoami`; gRPC health; ops port | Trigger intake, outbox relay, SQS dispatch, channel adapters, vendor callbacks |
| API key issue and authentication | Console (Next.js), sessions, OAuth, RBAC |
| Tenant-scoped store, migrations, audit trail | Channel credentials (BYO secrets), template rendering, webhook channel |
| Envelope encryption (`internal/crypto`), partition jobs | MCP server, Terraform actions, inbound webhooks, platform-operator realm |
| CI pipeline, container image | AWS deployment (doc 10) |

## 2. Assets (ranked by impact if compromised)

1. **Tenant notification data:** payloads, raw recipients (PII) and rendered content. Encrypted per tenant (ADR-006) and purged after 1–7 days.
2. **Vendor credentials:** BYO secrets, and the platform SES/SNS identities, whose abuse burns sender reputation and money.
3. **API keys and console sessions:** whoever holds one can trigger notifications as the tenant.
4. **Tenant isolation itself:** a cross-tenant read or write is a critical incident (charter §2.1).
5. **Sender reputation:** shared SES domains and SNS origination. One abusive tenant hurts every tenant.
6. **Audit integrity:** the record of who changed what.
7. **Availability:** the accept path and dispatch throughput.
8. **Key material:** KMS CMK, per-tenant DEKs, API key pepper.

## 3. Actors

| Actor | Capability | Motivation |
|---|---|---|
| Anonymous internet | Reaches the ALB/CloudFront: REST, gRPC, later console and MCP | Credential stuffing, DoS, key enumeration |
| Malicious tenant | Valid account and keys, full control of their own templates, payloads and webhook URLs | Read other tenants' data, spam or phishing through our reputation, SMS toll fraud, SSRF into our VPC |
| Compromised tenant credential | A leaked API key or session | Same as the tenant, within that credential's scope |
| Malicious or careless insider / platform operator | Operator realm, production access | Read tenant data, tamper with audit |
| Compromised dependency or vendor | Code execution in our build or runtime, or a lying vendor callback | Exfiltration, status forgery |
| Compromised CI | Pushes images, runs Terraform | Backdoors, secret theft |

## 4. Trust boundaries and data flows

```
internet ──TLS──► ALB/CloudFront+WAF ──► api (REST/gRPC) ──► Postgres (RDS)
                                            │                 ▲
                                            └──► SQS ──► dispatcher ──► vendors (SES, SNS, Twilio, Slack, Graph, webhooks)
console (browser) ──► web (Next.js) ──► api            vendors ──callbacks──► api
operator ──separate realm──► api (operator routes)     scheduler ──► Postgres (relay, purge, partitions)
CI/CD (GitHub OIDC) ──► ECR, ECS, Terraform
```

**B1** internet → api · **B2** console → api · **B3** api/dispatcher → Postgres and SQS · **B4** dispatcher → vendors and tenant webhook URLs · **B5** vendors → callbacks · **B6** operator → tenant data · **B7** CI/CD → production.

## 5. STRIDE by boundary

### B1: internet → api (REST, gRPC; MCP and webhooks later)

| Threat | Example | Built | Planned |
|---|---|---|---|
| **S**poofing | Forged or guessed API key | 256-bit secret, HMAC with pepper, constant-time compare (`internal/auth/apikey.go`). Prefix-binding test. One 401 for every failure, and a dummy HMAC on unknown prefixes, so neither error text nor timing reveals what failed (`authenticator.go`, `TestAuthenticateRejections`). **Per-IP limit on failed API-key attempts** (20/min, 429 + Retry-After), counting the client IP from `X-Forwarded-For` only when it arrives from a trusted proxy (`TestPerIPRateLimits`, `TestTrustedProxyForwardedFor`, mutation-tested) | WAF rate rule as a shared layer across tasks. OAuth for console and MCP (Phase 1 slice 3, doc 12) |
| **S** | Revoked or expired key still works | Checked on every request (`TestAuthenticateRejections`, the HTTP "revoked key is 401 immediately" test). No auth cache yet, so revocation is instant. A rotation grace period can always be cut short by revoking (`TestRevokeCutsGracePeriodShort`, mutation-tested). Developers can't touch prod keys (mutation-tested) | If an auth cache is added for performance (Phase 3), bound its TTL so revocation lands in ≤ 30 s (docs/05) |
| **S** | A dev key used against prod | The key text carries its environment, and it must match the stored environment record (mutation-tested) | Scope and workflow allowlists per key (doc 12, Phase 3) |
| **T**ampering | Request smuggling, oversized bodies | 1 MiB body cap on REST, also enforced while reading a streamed body; 1 MiB gRPC messages; header and read timeouts (`lifecycle.NewHTTPServer`) | WAF managed rules (doc 10) |
| **R**epudiation | Tenant denies triggering | Request ids. Correlation fields (`tenant_id`, `api_key_id`) on every authenticated log line and span | `source` + `actor` on every run (Phase 3) |
| **I**nformation disclosure | Error details or stack traces leak | Panics return a generic 500 (test asserts the panic value isn't in the body). Public `/readyz` hides dependency detail, which lives only on the ops port (never behind the ALB). Postgres DETAIL (key values) is kept out of errors (ADR-010) | OpenAPI-driven DAST and fuzzing (docs/06 §2) |
| **I** | Secrets or PII in logs | Logger redacts `authorization`, `api_key`, `token`, `payload`, `recipient`, … at any depth. Access logs record route templates, never raw paths or queries. Tests assert the key secret never appears in logs (`whoami_test.go`) | PII-grep test over captured logs from the full E2E suite (docs/05) |
| **D**enial of service | Flood or slowloris | Server timeouts. Malformed keys rejected by checksum without a DB query. Metric labels kept low-cardinality (unknown methods map to `OTHER`) | WAF rate limiting; per-tenant token bucket on trigger (Phase 3); autoscaling (doc 10) |
| **D** | DB outage turns every client away for good | Auth returns 503 + `Retry-After`, never 401, so clients retry. Liveness stays 200, so ECS doesn't kill every task on a DB blip (verified live) | — |
| **E**levation | Key used for control-plane actions | Only `/v1/whoami` exists today | RBAC matrix test for every role × capability; API keys never authorize `/v1/admin` (Phase 1) |

### B2: console → api (Phase 1, ADR-011)

| Threat | Built (Phase 1 slice 2) | Planned |
|---|---|---|
| Session theft or replay | Opaque `nfs_` tokens (256 bits), only SHA-256 stored, idle 12 h / absolute 7 d, immediate revocation (logout, revoke, reset revokes all). Tokens never logged (`TestConsoleJourney` greps logs) | Web app: `__Host-` HttpOnly SameSite cookie (Phase 6) |
| Token confusion | Each token kind accepted by exactly one route group: sessions are rejected on the data plane and API keys on console routes (`TestSessionsNeverAuthorizeTheDataPlane`, error-mapping test) | — |
| CSRF | API is bearer-only (no cookies), so it can't be targeted by CSRF directly | Web app Origin check + double-submit (Phase 6) |
| Credential stuffing / brute force | argon2id (46 MiB, concurrency-bounded); per-account progressive backoff in Postgres (race-free, shared across tasks, also for unknown emails); per-IP limits on login, signup, reset and verify; uniform `invalid_credentials` with dummy-hash timing (`TestLoginFailuresAreUniformAndBackOff`, `TestThrottleCountsConcurrentFailuresExactly`) | WAF rate rules; vetted breached-password dataset before GA |
| Account enumeration | Signup and forgot-password return identical responses for existing and unknown emails; an existing owner gets a "you already have an account" email (`TestConsoleResponsesDoNotRevealAccounts`) | — |
| Token leakage via links | Emailed tokens travel in the URL fragment (never sent to servers or logged); single use, atomic consume; a new reset invalidates older links | — |
| Mail bombing | At most 3 reset emails per address per hour, applied silently | Per-IP signup mail cap (Phase 1 slice 4 with invitations) |
| Email header injection | CR/LF in To/Subject/From rejected before sending (`TestHeaderInjectionRejected`) | — |
| Mass assignment / unexpected fields | Strict JSON decoding rejects unknown fields (mutation-tested) | — |
| OAuth account takeover | PKCE S256 + single-use, provider-bound `state` (10 min) + `nonce`; exact-match redirect allowlist; ID token signature, issuer, audience, expiry and nonce verified; Microsoft keyed on `tid:oid` with per-tenant issuer check, email trusted only with `xms_edov` (nOAuth); GitHub primary *verified* email only; unverified provider emails never looked up; auto-link only when both sides are verified; linking requires the session that started it (linking CSRF). Each rule covered by a failing-on-mutation test against a fake IdP (`oauth_test.go`) | Real-provider smoke test in staging |
| XSS through tenant content | — | Template preview in a sandboxed iframe with strict CSP (docs/11) |
| BOLA across tenants | Path-scoped `/v1/admin/tenants/{id}`: anything but a live membership is `404`, including malformed and nil ids (`TestTenantAdminBOLA` sweeps every tenant endpoint as a member of another tenant). Capabilities enforced in the service layer from a literal role × capability matrix test. Last-owner and tenant-cap invariants hold under concurrency (row locks, mutation-tested). Invitations are email-bound, single use and expiring, and a wrong-account accept doesn't consume the link | BOLA sweep extended to every endpoint as later slices add them (slice 7) |

### B3: services → Postgres / SQS (tenant isolation)

| Threat | Example | Built | Planned |
|---|---|---|---|
| **I/T** cross-tenant read or write | A store method forgets `tenant_id` | Explicit `TenantID` on every method. ent interceptor and hook add `tenant_id = scope` and **fail closed** with no scope (`TestScopeEnforcementFailsClosed`, mutation-tested). Isolation table covers every tenant-scoped method (`TestTenantIsolation`). Cross-tenant ids return 404 | Postgres RLS as a 4th layer (Phase 8); docs/06 BOLA suite against every endpoint as they ship |
| **T** cross-tenant references | Tenant B's environment pointing at tenant A's app | Composite FKs reject it at the database, even from raw SQL (`TestDatabaseRejectsCrossTenantReferences`) | — |
| **T** mass assignment | `tenant_id` in a request body | The store takes the tenant from the authenticated principal, never from input. Scoped creates for another tenant fail | Request DTOs never expose `tenant_id` (Phase 1 review checklist) |
| **I** stolen DB snapshot or backup | Read payloads | RDS encryption (planned) plus per-tenant AES-256-GCM DEKs. AAD binds tenant, row and **column**, so ciphertexts can't be moved between cells. Wrapped DEK bound to its tenant via encryption context (`internal/crypto` tests, fuzz) | KMS CMK; DEK cache TTL is the cross-instance shred window (5 min) |
| **I** PII in queue messages | Payload ends up in SQS | Envelope carries ids only (ADR-004, contract) | Envelope schema test rejecting any extra fields (Phase 3) |
| **R** audit tampering | Erasing history | Append-only store API (no update or delete); audit written in the same tx as the mutation (`TestWithTx`) | Separate DB role with INSERT-only rights on `audit_events`; export to S3 Object Lock (Phase 8) |
| **D** runaway queries | Unbounded list | Every list is keyset-paginated with a max of 200. Partition DDL runs under `lock_timeout` | Statement timeout per role (Phase 3) |
| **E** SQL injection | Dynamic DDL | ent parameterizes queries. Partition DDL identifiers are regex-validated and quoted (`TestValidation`) | semgrep rules in CI (docs/06) |

### B4: dispatcher → vendors and tenant URLs (Phase 2–4)

| Threat | Mitigation (planned) |
|---|---|
| **SSRF** via the webhook channel or logo fetches | Resolve-then-connect allowlist; block RFC1918, loopback, link-local, metadata endpoints and their IPv6 forms; no redirects to internal targets; egress proxy; size and time limits (docs/06 §3) |
| **Spoofing our identity** (spam or phishing through platform SES/SNS) | Sandbox tenants by default (built: new tenants are always sandboxed, and only an operator can lift it). Verified sender domains, bounce and complaint auto-suspension, per-tenant daily SMS limit enforced atomically at accept (ADR-005) |
| **SMS toll fraud / pumping** | Country allow/deny lists, per-tenant daily caps plus a platform cap below the AWS spend quota, velocity alerts |
| **Credential leakage** | BYO secrets envelope-encrypted, decrypted only in the dispatcher at send time, never logged, write-only in the API (ADR-005) |
| **Template injection (SSTI)** | Sandboxed renderer: no function calls or I/O, time and size limits, fuzzing (Phase 2) |
| **Header injection** | CRLF rejection in subject, sender and recipient fields (Phase 2) |

### B5: vendor callbacks → api (Phase 4)

| Threat | Mitigation (planned) |
|---|---|
| Forged delivery status (false "delivered", billing manipulation) | Signature verification per provider (SNS message signing, Twilio, SES via SNS), replay window, idempotent on `notification_id`; `usage_records` unique per notification (ADR-007) |
| Callback flood | Per-provider rate limit, cheap signature check before any DB work |

### B6: platform operator → tenant data (Phase 1+)

| Threat | Built (Phase 1 slice 6) | Planned |
|---|---|---|
| Insider reading payloads | Separate realm: `operators` and `operator_sessions` tables, `nfo_` tokens accepted only on `/v1/operator` (and rejected everywhere else; tested). Operators have no payload, template or credential endpoints at all | Payload decryption requires KMS permissions held only by the dispatcher role, never operators (Phase 8 IAM) |
| Operator account takeover | Password (argon2id) **and** TOTP on every login; a TOTP code is single use (atomic `totp_last_step` advance; 8 concurrent logins with one code → exactly one session, tested); uniform `401` for every failed factor; per-account backoff plus 5/min per IP; sessions idle 30 min, absolute 8 h; `disable` revokes every session at once | WAF IP allowlist or VPN for `/v1/operator` (Stage 2); WebAuthn |
| Rogue operator account | No signup endpoint: accounts are created only by `cmd/operator` from a trusted terminal. The TOTP secret is shown once and stored sealed (AES-256-GCM, platform key via `KeyProvider`, context bound to the operator id) | Two-person approval for `operator create` (Stage 2) |
| Snooping without trace | **Every** operator request is audited with actor type `operator`, reads included, and misses on unknown tenant ids too. A read whose audit write fails returns no data | Audit-log export and alert on unusual operator volume (Phase 7) |
| Silent config changes | Status, plan, sandbox, `platform_sms_daily_limit` and quota changes require a `reason`; the audit event stores before, after and reason in the same transaction. Operators cannot delete a tenant (owner-only crypto-shred) or change a deleted one | Two-person review for platform SMS cap changes |
| CLI output leaking the TOTP secret | `cmd/operator` prints it to the operator's terminal only; documented: never run it as an ECS task (CloudWatch would keep it) | — |

### B7: CI/CD → production

| Threat | Built | Planned |
|---|---|---|
| Malicious dependency | `go.sum` pinning; govulncheck; Trivy fails on HIGH/CRITICAL; base images pinned by digest; tool versions pinned in the Makefile | Pin GitHub Actions by SHA (TODO in `ci.yml`); SBOM + cosign + SLSA provenance (Stage 2) |
| Secret leak in repo | gitleaks in CI; `.env*` gitignored; dev placeholder secrets only used with `NOTIFEX_ENV=local` and rejected elsewhere (config tests) | Secret scanning push protection on GitHub |
| Long-lived cloud credentials | — | GitHub OIDC → scoped IAM roles, protected environments (doc 10) |
| Unreviewed schema change | Migrations are generated and reviewed files; `migrate-check` and squawk in CI; forward-only `migrate` refuses dirty databases | Migration dry-run on a prod-sized copy (charter §6) |

## 6. Residual risks (likelihood × impact)

| # | Risk | L | I | Status / next action |
|---|---|---|---|---|
| R1 | ~~No rate limit on failed API-key auth~~ **Mitigated (Phase 1 slice 2):** per-IP limiter on failed API-key attempts (per task); WAF rule still to add as the cross-task layer | L | M | WAF rate rule with the AWS Terraform (Phase 8) |
| R2 | **Shared-sender abuse once real vendors exist** (spam, phishing, SMS pumping) | H | H | Phases 2–3: verified domains, caps, suspension hooks before Stage 2 (real vendors) |
| R3 | **SSRF through the webhook channel** | M | H | Phase 3: egress allowlist proxy + resolve-then-connect, with tests from docs/06 |
| R4 | **CI has never run on GitHub;** squawk has never checked the migrations | M | M | Open a PR from `chore/reset-to-new-design` |
| R5 | **GitHub Actions pinned by tag, not SHA** | L | H | Stage 2 TODO; Dependabot for actions |
| R6 | **Crypto-shred takes up to one DEK-cache TTL (5 min) on other instances** | L | M | Accepted (ADR-006); tenant deletion also stops processing first |
| R7 | **Backups (PITR) keep payloads longer than tenant retention** | M | M | Accepted; covered by crypto-shred; disclose in the DPA (ADR-006) |
| R8 | **No RLS yet;** isolation relies on app-layer enforcement plus composite FKs | L | H | Phase 8 RLS; meanwhile the isolation and scope tests are mandatory for new store methods |
| R9 | **Auth makes 3 DB queries per request;** caching would add a revocation lag | M | L | Measure in Phase 3 load tests; if caching, bound TTL ≤ 30 s |
| R10 | **External pen test not done** | — | — | Stage 3 gate (charter §5); out of scope for automated work |

## 7. Review triggers
Update this document whenever any of these lands: a new external surface (endpoint group, protocol, callback), a new data store or queue, a new class of secret, a new tenant-supplied input that is rendered, fetched or executed, or a change to the operator realm. Every PR that adds a surface links the section it changes.
