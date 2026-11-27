# 08 — Launch Plan (MVP → Private Beta → Public Beta → GA)

Principle: earn trust in stages. Each stage has a go/no-go gate (see ENGINEERING_CHARTER.md §5). Dates are relative; adjust to capacity.

## Positioning hypothesis (validate with design partners)
"Notification infrastructure you can trust and see into: configure once, trigger from anywhere (SDK, REST, gRPC), and watch every notification end to end, with each of your tenants' brand and policies enforced automatically."
Candidate differentiators (test, don't assume): per-notification observability and replay; first-class multi-tenant brand and policy; infrastructure-as-code (Terraform provider); gRPC and SDK-first ergonomics; transparent, predictable pricing; strong coverage for India-specific channel compliance (SMS DLT, WhatsApp). See `09-competitive-landscape.md`.

## Ideal customer profiles (ICP)
1. **B2B SaaS teams (10–200 engineers)** that send transactional email/SMS/Slack/WhatsApp and have outgrown home-grown queues, or are paying for a vendor whose pricing/observability frustrates them.
2. **Platforms with their own customers (multi-tenant SaaS)** needing per-customer brand and policy.
3. **Indian and APAC product teams** with SMS/WhatsApp-heavy flows and compliance overhead.
Anti-ICP at launch: marketing-campaign teams, consumer apps needing complex in-app inbox/preference centers (non-goals in v1).

## Timeline

### T-12 to T-8 weeks — Alpha (internal)
- Dogfood: run Notifex for your own product notifications and side projects.
- MVP scope complete (see docs/01). Mock vendors + one real email and one real SMS vendor.
- Threat model v1, scanners in CI, smoke load test in CI.
- **Gate:** Stage 1 exit criteria.

### T-8 to T-4 weeks — Private beta (5–10 design partners)
- Recruit design partners: founder network, LinkedIn, India startup communities, ex-colleagues, r/SaaS-style forums. Offer free usage and direct Slack access in exchange for weekly feedback and a case study.
- White-glove onboarding (<1 hour to first notification). Measure time-to-first-notification (TTFN).
- Real vendors on all launch channels; status page live; on-call rota; runbooks.
- First security assessment + 2× load test; fix criticals/highs.
- **Gate:** Stage 2 exit criteria; ≥3 partners sending production traffic; zero Sev1 in the final two weeks.

### T-4 to T-0 — Public beta preparation
- External penetration test and remediation; 5–10× load, soak, spike, failover drills.
- Docs site (quickstarts per SDK, API reference, guides: migrating from X, multi-tenant setup, going to production checklist), changelog, SLA draft, DPA, privacy policy, sub-processor list, ToS and acceptable-use policy (anti-spam).
- Pricing and billing live (Stripe or Razorpay for INR), usage metering verified against ground truth.
- Marketing site, demo video (3 min), sample apps (Go, Node, Python), Terraform registry listing, SDK packages published.
- **Gate:** Stage 3 exit criteria; support process tested; rollback rehearsed.

### Launch week (public beta)
- Day 0: announce on Product Hunt, Hacker News (Show HN), dev.to/Hashnode post ("How we built X: dispatch with SKIP LOCKED, idempotency, multi-tenancy"), LinkedIn/X, relevant Slack/Discord communities, India dev communities.
- Day 0–7: live support, daily metrics review, daily fix releases, public status page updates, thank-you notes to design partners with testimonials.
- Content: benchmark post with honest load-test numbers; comparison pages ("Notifex vs Novu/Knock/Courier/SuprSend") written factually and sourced.

### T+1 to T+3 months — Public beta to GA
- Weekly releases; NPS/CSAT surveys; churn interviews; top-10 feature requests triage against non-goals.
- SOC 2 Type I started; SLA published; enterprise features (SSO/SAML, audit export, regional data residency) as demanded.
- GA announcement when: SLOs met 60 consecutive days, no open critical/high security findings, ≥N paying customers (set N), support response time targets met.

## Pricing approach (hypothesis)
- Free tier (e.g. 10k notifications/month) to remove friction, since competitors cluster here (see doc 09).
- Usage-based paid tiers per accepted notification with predictable bundles; flat platform fee for team features; enterprise custom (SSO, SLA, residency, BYO vendor credentials).
- Make billing **simple and explainable**: avoid multi-meter surprises. Publish a cost calculator.
- Pass-through vendor costs transparently (SMS/WhatsApp carrier fees) or support BYO vendor accounts.

## Channels to acquire customers
- Developer content and SEO (build-vs-buy, migration guides, "how to design idempotent notification systems").
- Open-source adjacent assets: SDKs, Terraform provider, sample repos, a CLI.
- Integrations/marketplaces: AWS Marketplace, Terraform Registry, vendor partner directories.
- Founder-led outbound to ICP 2 (multi-tenant platforms) and Indian SaaS.
- Community: answer notification-architecture questions where developers ask them.

## Launch metrics
Activation: signup → first successful trigger within 24h (target >50% for qualified signups); TTFN median <15 min self-serve.
Reliability: accept availability, dispatch lag p95, delivery success, incident count and time to resolve.
Business: weekly active tenants, notifications/month, MRR, logo and net revenue retention, CAC payback, gross margin (after vendor and infra cost).
Support: first-response time, tickets per 100 tenants, docs search failures.

## Go/no-go checklist for each launch
- [ ] SLOs met in the latest load test run (link report)
- [ ] No open critical/high security findings (link report)
- [ ] Backups restored successfully in the last 30 days
- [ ] On-call rota staffed, runbooks reviewed, alerts tested
- [ ] Status page, support inbox, and escalation path live
- [ ] Legal: ToS, AUP, privacy policy, DPA ready
- [ ] Billing and metering reconciled
- [ ] Rollback and feature flags verified
- [ ] Docs and quickstarts tested by someone who didn't write them
- [ ] Comms plan and customer-facing incident templates ready

## Risks and mitigations
| Risk | Mitigation |
|---|---|
| Crowded market, buyers default to Novu/Knock/Courier | Narrow ICP, sharp differentiation (observability, tenancy, IaC, pricing), migration tooling |
| Trust: a missed notification damages customers | SLO discipline, replay, transparency, honest status page |
| Vendor dependency and cost volatility | Multi-vendor failover per channel, BYO credentials, margin tracking |
| Spam/abuse harms sender reputation | Domain verification, ramp limits, content/bounce monitoring, fast suspension |
| Solo-founder bandwidth | Automate (CI gates, runbooks), keep scope tight, defer enterprise features |
| Compliance blockers for enterprise | Start SOC 2 early, DPDP/GDPR readiness, regional options |
