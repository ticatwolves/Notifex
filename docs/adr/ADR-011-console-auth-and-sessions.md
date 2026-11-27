# ADR-011 — Console authentication, sessions, tenants and the operator realm

- **Status:** Accepted (2026-11-27). Approved with the Phase 1 plan.
- **Decider:** Shobhit
- **Supersedes:** docs/11 "short-lived access + refresh handled by the Go auth service" (JWT). This ADR uses opaque server-side sessions instead.

## Context
Phase 1 adds human users: signup, login, OAuth, tenants, members, roles, and a platform-operator realm. The console (Next.js, ADR-003) is a separate deployable, and only the Go API decides authorization. docs/05 requires that removing a member revokes their access immediately, and that every role × capability pair is tested.

## Decisions

### 1. Opaque server-side sessions, not JWTs
- A login creates a session: `nfs_` + 43 base62 characters (256 random bits). Only the **SHA-256** of the token is stored (`sessions.token_hash`). A pepper adds nothing for 256-bit random tokens.
- The API authenticates `Authorization: Bearer nfs_…`. The prefix keeps session tokens (`nfs_`), operator sessions (`nfo_`) and API keys (`nfx_`) apart, and each route group accepts exactly one kind.
- **Expiry:** idle 12 h, absolute 7 days. `last_seen_at` is written at most once a minute.
- **Revocation is immediate:** logout, the "sign out other sessions" endpoint, and password change or reset (which revokes every other session). Membership changes need no session revocation, because roles are loaded on every request (nothing is cached), so a removed member loses tenant access on their next request.
- **Why not JWT:** a JWT stays valid until it expires, so revoking one means keeping a deny-list, which is a sessions table under another name. Console traffic is low, and the cost here is one indexed lookup per request.

**Cookie contract for the console (Phase 6).** The web app keeps the token in `__Host-nfx_session` (HttpOnly, Secure, SameSite=Lax, Path=/, no Domain) and forwards it as a bearer token from server components and route handlers. The browser never sees the token in JavaScript. **CSRF is enforced by the web app** on every state-changing request: an Origin check plus a double-submit token. The API is not cookie-authenticated, so it can't be targeted by CSRF directly.

### 2. Accounts and credentials
- **Signup creates the user only.** `POST /v1/auth/signup` always returns 202 (whether or not the email exists, to prevent enumeration) and sends a verification email. An unverified user can log in but can't create a tenant or accept an invitation sent to another address.
- **Passwords:** argon2id with OWASP parameters (m=46 MiB, t=1, p=1), stored as a PHC string and rehashed on login when the parameters change. Hashing runs behind a **concurrency limit** (default 4 per task, about 184 MiB peak), so a login flood can't exhaust memory on small Fargate tasks. Length 12–128 characters, checked against an embedded list of common passwords. No external breach API: that would send derived user data to a third party.
- **Tokens for email verification, password reset and invitations:** 32 random bytes, stored as SHA-256, single use. Lifetimes: verification 24 h, reset 1 h, invitation 7 days.
- **Brute force:**
  - per IP: 20 login attempts per minute;
  - per account: after 5 failures, progressive backoff (2ⁿ seconds, capped at 15 minutes), reset by a successful login. A failed attempt never hard-locks the account, so an attacker can't lock a victim out.
  - Both return `429 account_locked` / `rate_limited` with `Retry-After`. The same per-IP limiter covers API-key 401s (threat model R1).
  - Every login failure returns the same `401 invalid_credentials`. For unknown emails, a dummy argon2 hash keeps the timing the same.
- **Re-authentication for sensitive actions:** deleting a tenant, transferring ownership and promoting someone to owner require a session authenticated within the last 10 minutes. Otherwise the API returns `403 reauth_required`, and the client calls `POST /v1/auth/reauth`.

### 3. OAuth (Google, Microsoft, GitHub)
- **Authorization code flow with PKCE, `state` and `nonce`.** `POST /v1/auth/oauth/{provider}/authorize` returns the provider URL. The `state`, PKCE verifier and `nonce` are stored server-side for 10 minutes, are single use, and are bound to the provider and an **allowlisted** `redirect_uri`. The web app receives the provider's redirect and posts `code` + `state` to `/callback`.
- **Identity verification:** Google ID token (`iss`, `aud`, `exp`, `nonce`, `email_verified`). Microsoft identities are keyed on `tid` + `oid`. **Microsoft's `email` claim is never trusted for linking** (it is mutable: the nOAuth class of takeover); a Microsoft email counts as verified only with the verified-domain-owner claim. GitHub uses the primary email from `/user/emails`, and only if it is verified.
- **Linking rule:**
  - a known `(provider, subject)` logs in;
  - an unknown one with a provider-verified email matching a user whose email is also verified gets linked;
  - an email that matches but isn't verified on both sides returns `409 link_required`, and the user must log in first and link from their settings;
  - no match creates a user (verified if the provider says so).

### 4. Tenants, members and RBAC
- **Creating a tenant is a separate step:** `POST /v1/admin/tenants` by a verified user, who becomes its owner. Limited to 5 tenants per user, to curb abuse.
- **Routes are tenant-scoped by path:** `/v1/admin/tenants/{tenant_id}/…`. The handler resolves the caller's membership for that tenant on every request. No membership means `404` (the tenant's existence isn't revealed). Insufficient role means `403 forbidden`.
- **Capabilities** come from the docs/03 matrix and live in code: a fixed list plus a static role → capability table, enforced in the **service layer**. A table-driven test covers every role × capability pair, and every endpoint gets a deny test.
- **Membership rules:**
  - the last owner can't be removed or demoted;
  - admins can't change or remove owners;
  - nobody can grant a role above their own;
  - only an owner can invite or promote another owner;
  - ownership transfer is owner-only and requires re-authentication.
- **Invitations** are bound to an email address. Accepting one needs a session whose user email matches; accepting also counts as verifying that email.
- **API keys:** developers can create keys for non-prod environments only. Admins and owners can create any. Rotation creates a new key and revokes the old one after a grace period the caller sets (default 24 h, at most 7 days).
- **Tenant deletion** is owner-only and requires re-authentication. It sets status `deleted`, crypto-shreds the DEK (ADR-006) and revokes every API key, all audited. It can't be undone.
- **API keys never authorize `/v1/admin`,** and sessions never authorize the data plane (`/v1/trigger`, etc.).

### 5. Operator realm
- **Separate accounts** in an `operators` table, created only by a CLI (`notifex-operator create`). There's no signup endpoint.
- **TOTP is mandatory** (RFC 6238, 30 s step, ±1 step tolerance, a used step can't be replayed). The TOTP secret is envelope-encrypted under a platform key via `KeyProvider`, never under a tenant DEK.
- **Operator sessions** use the `nfo_` prefix, a separate `operator_sessions` table, idle 30 min and absolute 8 h, and their own rate limits.
- **Routes** live under `/v1/operator/…` and accept only `nfo_` tokens. **Every operator request is audited, reads included,** with actor type `operator`.
- **Phase 1 operator capabilities:** list and inspect tenants, and change status (suspend or reactivate), plan, sandbox, `platform_sms_daily_limit` and quotas. Operators cannot read payloads: the KMS decrypt permission belongs only to the dispatcher role (threat model B6).

### 6. Audit
Every control-plane mutation is audited in the same transaction (ADR-010 helpers). So are security events: login success, login failure (no password, email masked), logout, session revocation, password change or reset, OAuth link and unlink, re-authentication, and every operator request.

## Consequences
- **New tables:** `sessions`, `operator_sessions`, `operators`, `user_identities`, `email_tokens` (verify and reset), `invitations`, `oauth_states`, `auth_throttle`. Each gets the store tests and, where tenant-owned, the isolation tests.
- **New public id kinds:** `usr_`, `mem_`, `inv_`, `ses_`, `evt_`.
- **System email** goes through a `Mailer` interface: Mailpit in compose for local dev, SES later. Tests use a recording mailer.
- **docs/11** (Next.js section) now points at this ADR. Phase 6 must implement the cookie and CSRF contract above.
- **The threat model's B2 (console) and B6 (operator) sections** become "built" as slices land.

## Alternatives considered
- **JWT access + refresh** (the old docs/11 text): revocation lag or a deny-list. Rejected.
- **Creating the tenant at signup:** couples identity to a tenant and makes joining by invitation awkward. Rejected.
- **Header-based tenant selection** (`X-Tenant`): implicit, easy to forget in a proxy, and harder to audit. Rejected in favor of the path.
- **Operators as a tenant role:** mixes realms, so a tenant bug could escalate to the whole platform. Rejected.
- **Hard account lockout after N failures:** lets anyone lock a victim out. Rejected in favor of progressive backoff.

## Amendment (2026-11-27, Phase 1 slice 3): OAuth details settled in implementation
- **An unverified provider email is rejected before any lookup** (`403 email_not_verified`). §3 had said "no match creates a user (verified if the provider says so)". But an unverified email that matched an existing account could only be answered with `409 link_required`, and that would let anyone with a provider account that asserts arbitrary unverified emails (Microsoft) discover which emails have Notifex accounts. New OAuth accounts therefore need a provider-verified email. Existing users can still attach such an identity from settings, where the session, not the email, proves ownership.
- **Linking from settings reuses `authorize` and `callback`, so the contract gains no extra endpoints.** Calling `authorize` with a session records that user in the OAuth state, and the callback then links instead of logging in, but only when it presents the **same** session. This blocks linking CSRF, where an attacker gets a victim's browser to attach the attacker's provider account to the victim's Notifex account. A presented but invalid session is a 401, never "anonymous", so a link attempt can't silently turn into a login. Added `GET /v1/me/identities` and `DELETE /v1/me/identities/{provider}`. Unlinking refuses to remove the last way to sign in.
- **Identities are keyed on the provider's stable subject** (Google `sub`, Microsoft `tid:oid`, GitHub numeric id), never the email. A user has at most one identity per provider.
- **No OIDC discovery at startup.** Endpoints are configured statically and signing keys are fetched lazily, so API boot never depends on Google or Microsoft being reachable. The Microsoft app registration must request the `xms_edov` optional claim; without it, Microsoft sign-in can still link from settings but cannot create accounts or auto-link.

## Amendment (2026-11-27, Phase 1 slice 4): tenants, members and invitations
- **Authorization is two-layered.** HTTP resolves the caller's membership in the path's tenant, and anything other than a live membership (malformed id, nil id, unknown tenant, deleted tenant, non-member) is the same `404`. The **service layer** checks capabilities against the fixed role → capability table in `internal/rbac`, whose test spells out the expected matrix literally.
- **Owner invariants under concurrency.** Every change that could remove an owner first locks the tenant's owner rows (`SELECT … FOR UPDATE`) in its transaction. The 5-owned-tenant cap is serialized by locking the user row. Both locks are mutation-tested: without them, the concurrency tests fail.
- **Re-inviting replaces.** Inviting an email that has a live invitation revokes it and issues a new link ("resend"), instead of returning `409`. A partial unique index keeps one live invitation per email per tenant. Inviting an existing member is `409 conflict`.
- **Accepting is atomic and email-bound.** Consuming the token, checking that the session's email matches, and creating the membership happen in one transaction. A mismatch rolls back, so the link stays usable by the right person. Accepting verifies the email, because the link proved the address.
- **Deletion** marks the tenant deleted, revokes every API key and crypto-shreds the DEK in one audited transaction. Afterwards the tenant is `404` to everyone, its invitations can't be accepted, and it no longer counts toward the owner's cap.
- **No KMS yet (Phase 8).** Deployed environments run with a KeyProvider that refuses to create DEKs, so tenant creation fails there while the rest of the API keeps working. The local master key is never used outside `local`.

## Amendment (2026-11-27, Phase 1 slice 6): operator realm
- **Provisioning is `cmd/operator create|disable`** (built as `operator`). It reads the password from stdin and prints the TOTP enrollment URI exactly once. It runs from a trusted terminal against the database, **never as an ECS task**, whose stdout would land in CloudWatch. Until the KMS KeyProvider exists (Phase 8) it refuses to run outside `local`. `disable` revokes every session of that operator in the same transaction.
- **TOTP:** implemented in-house (RFC 6238, HMAC-SHA1, 6 digits, 30 s, ±1 step), tested against the RFC vectors, so no dependency is added. The secret (160 bits) is sealed with `crypto.Seal`: a fresh data key from `KeyProvider`, AES-256-GCM, encryption context `{purpose: operator-totp, operator_id}`. It is a platform key, never a tenant DEK, so crypto-shredding a tenant can't lock operators out.
- **Replay protection is a conditional update:** `UPDATE operators SET totp_last_step = $step WHERE id = $id AND totp_last_step < $step`. Zero rows means the code (or an older one) was already used. This also kills the previous step's code after a login, at the cost of one operator not being able to log in twice within 30 s.
- **Login failures are uniform:** unknown email (dummy hash), wrong password, wrong or replayed code and disabled account all return the same `401 invalid_credentials`. A wrong password never reaches TOTP verification, so the code is only consumed by a fully successful login. Per-account backoff uses the shared throttle table under an `operator-login:` key; the per-IP limit is 5/min, separate from console login.
- **Every request is audited, reads included.** Search records the query, filter and result count. A view of an unknown tenant id is recorded as a platform-level event (`found: false`). If the audit write fails, the read returns an error, not data.
- **Tenant changes require a `reason`** (1–500 characters) and at least one field. Status can only be `active` or `suspended`, and deleted tenants are `404`. The spec's `OperatorTenantPatch` now has `required: [reason]`.
