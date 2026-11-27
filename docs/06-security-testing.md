# 06 — Security and Vulnerability Testing Plan

Goal: demonstrate, with evidence, that a tenant's data and a tenant's reputation (their sender identity) cannot be compromised through Notifex. Standards: OWASP ASVS L2, OWASP Top 10, OWASP API Security Top 10, CIS benchmarks for cloud/container.

## What Claude can and cannot do
Claude Code can run scanners, write and run attack-scenario tests against the local/staging stack, and fix findings. It cannot replace an independent external penetration test, review cloud account posture it has no access to, or certify compliance. Report these as gaps explicitly.

## 1. Threat model (STRIDE) — create `docs/threat-model.md`
Assets: tenant data (recipients, payloads), vendor credentials, API keys, sender reputation (domains, WhatsApp/SMS sender IDs), availability, audit integrity.
Actors: anonymous internet, malicious tenant, compromised tenant account, malicious insider, compromised dependency/vendor.
Trust boundaries: internet → API; console → API; API → DB/queue; worker → vendors; platform operator → tenant data; CI/CD → production.
Update on every new surface.

## 2. Automated scanning (CI gates)
| Class | Tools | Gate |
|---|---|---|
| Secrets | gitleaks / trufflehog | any finding fails PR |
| SAST (Go) | gosec, staticcheck, semgrep (go, owasp rules) | high+ fails |
| SAST (TS) | semgrep, eslint-plugin-security | high+ fails |
| Dependencies | govulncheck, npm audit / osv-scanner | critical/high with reachable fix fails |
| Containers | trivy / grype | critical fails; base images pinned and minimal |
| IaC | tfsec / checkov / trivy config | high fails |
| DAST | OWASP ZAP (API scan with OpenAPI), nuclei templates | nightly; high+ opens ticket |
| API fuzz | schemathesis / RESTler on OpenAPI; gRPC fuzz via grpc tooling | nightly |
| Supply chain | SBOM (syft), signed images (cosign), SLSA provenance, lockfile review | release |

## 3. Attack-scenario test suite (automated where possible, run against the full stack)

### Tenancy and authorization (highest priority)
- **BOLA/IDOR:** with tenant A credentials, enumerate and mutate every ID-bearing endpoint using tenant B's IDs (runs, notifications, workflows, templates, keys, members, brand, policy, audit). All must 404/deny. Include nested routes and list filters.
- **Broken function-level auth:** viewer/developer calling admin and owner endpoints; platform-operator endpoints reachable from tenant realm.
- **Mass assignment:** supply `tenant_id`, `role`, `status`, `created_by` in request bodies; must be ignored or rejected.
- **API key scope bypass:** dev-environment key against prod; app A key against app B workflow.
- **Privilege escalation via invites and OAuth account linking** (email-verification and account-takeover checks).

### Authentication and session
- Credential stuffing/brute force protection, enumeration via timing or error text, password reset token reuse/expiry, session fixation, cookie flags (`HttpOnly`, `Secure`, `SameSite`), CSRF on all state-changing console endpoints, JWT alg confusion/none/expiry (if JWTs used), OAuth `state`/PKCE enforcement, open redirect in callback URLs.
- API keys: high entropy, stored hashed, constant-time compare, rotation, revocation propagates in seconds, no keys in URLs or logs.

### Injection and untrusted content
- **Template injection (SSTI):** payloads in templates and trigger payloads attempting code execution, resource exhaustion, data exfiltration of other tenants' variables.
- **Email header injection:** CRLF in subject, recipient, sender name, reply-to.
- **Stored/reflected XSS:** template preview, run explorer rendering payloads, notification detail views, brand fields (logo URL, colors), error messages.
- **SQL/ORM injection** on every filter/sort parameter (ent parameterizes, but test raw queries and dynamic ordering).
- **Log injection** (newlines/ANSI in user-controlled fields).
- **Content spoofing/phishing via brand fields**: validate and restrict logo URLs and links; consider domain verification for sender identities.

### SSRF and outbound abuse
- Webhook channel and logo/asset fetching: block RFC1918, loopback, link-local/cloud metadata (169.254.169.254), IPv6 equivalents, DNS rebinding, redirects to internal targets, non-HTTP schemes. Use an egress proxy with an allowlist model and resolve-then-connect validation.
- Response size, time, and redirect limits.

### Abuse, fraud, and business logic
- **Spam/open-relay abuse:** tenant using Notifex to send phishing/spam; require verified sender domains, new-tenant sending limits, content and bounce/complaint monitoring, automatic suspension hooks.
- **SMS/WhatsApp pumping (toll fraud):** premium-number and country allow/deny lists, per-tenant spend caps, velocity checks, anomaly alerts.
- **Quota/rate-limit bypass:** many API keys, many IPs, header spoofing (`X-Forwarded-For`), batch endpoint amplification, scheduled-trigger flooding.
- **Replay:** reuse of signed callbacks, idempotency key abuse, race conditions on key creation/revocation and quota counters.
- **Resource exhaustion:** giant payloads, deeply nested JSON, huge recipient arrays, template expansion bombs, expensive filters on the runs explorer.

### AI agent, MCP, Terraform and webhook surfaces
- **Prompt-injection and confused-deputy:** content returned by other tools instructs the agent to message arbitrary recipients; verify server-side recipient policy, approval modes, and caps stop it regardless of client behavior.
- **Over-broad OAuth:** token for app A used on app B, wrong audience/resource, scope upgrade via refresh, consent bypass, open redirect in OAuth callbacks, client-impersonation via CIMD/DCR (redirect URI validation, metadata fetch SSRF), issuer mix-up.
- **MCP transport:** missing Origin validation (DNS rebinding), unauthenticated discovery leaks, tool-argument injection (template/header injection through `subject`, `body`), oversized or deeply nested arguments, tool-result poisoning (returned strings that instruct the model).
- **Credential abuse:** leaked agent key used from elsewhere (IP allowlist, anomaly suspension), key reuse across environments, caps bypass through many keys, kill switch latency.
- **Terraform path:** CI secret exposure in logs/state, plan-time information leaks, workflow-exposure bypass, idempotency-key collisions allowing suppression of alerts.
- **Inbound webhooks:** forged signatures, replay, mapping-template escape, SSRF via mapping, enumeration of hook ids.
- **Spend and reputation:** SMS/WhatsApp pumping through agent credentials, external-recipient spam via direct send, brand/sender spoofing in direct-send content.

### Data protection
- Vendor credentials and payloads encrypted at rest; TLS 1.2+ everywhere (HSTS, no weak ciphers); internal service-to-service TLS where applicable.
- Secrets absent from logs, traces, error responses, client bundles, and container layers.
- PII masked in observability data; payload redaction after retention window.
- Backups encrypted; restore access restricted and audited.

### Infrastructure and operations
- Least-privilege IAM per service; no wildcard policies; separate prod/non-prod accounts.
- DB not publicly reachable; network segmentation; security groups reviewed via IaC scan.
- Admin/ops endpoints (`/metrics`, `/debug`, pprof) not exposed publicly.
- Security headers on console (CSP without `unsafe-inline` where possible, `frame-ancestors`, `nosniff`, `Referrer-Policy`, `Permissions-Policy`).
- CI/CD: branch protection, required reviews, OIDC to cloud (no long-lived keys), protected deploy environments, dependency pinning.

## 4. Manual and external testing
- Internal manual pass with Burp/ZAP proxy on the top abuse scenarios before beta.
- **Independent penetration test** (grey-box, with source and test tenants) before GA; scope: tenancy, auth, SSRF, template engine, cloud config. Re-test after remediation.
- Bug bounty / responsible-disclosure policy (`security.txt`) at GA; private invite-only program first.
- Optional: third-party code review of tenancy layer and renderer sandbox.

## 5. Findings management
- Severity via CVSS plus business context. SLA: critical 24h, high 7d, medium 30d, low backlog.
- Every finding: description, affected component, reproduction, impact, fix, **regression test**, retest evidence.
- Report format: executive summary, scope, method, findings table, residual risks, items not tested.
- Gate: zero open critical/high before beta expansion and GA; medium findings need a dated plan.

## 6. Compliance track (parallel)
- SOC 2 Type I → Type II preparation (policies, access reviews, change management, vendor management); ISO 27001 later if enterprise demand.
- Privacy: India DPDP Act and GDPR readiness (DPA template, sub-processor list, data residency options, retention and deletion, breach notification process).
- Channel compliance: TRAI DLT registration for SMS in India, WhatsApp Business policy, CAN-SPAM/CASL and unsubscribe handling are tenant responsibilities but Notifex must provide the controls and evidence.
