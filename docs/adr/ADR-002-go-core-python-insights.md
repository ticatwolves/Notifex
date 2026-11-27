# ADR-002: Go owns the core platform; Python owns insights and tooling
Status: Accepted

**Context.** Team knows Go and Python. The product's trust depends on a small, fast, strongly typed core; analytics/ML/test tooling benefit from Python's ecosystem.
**Decision.** Go for trigger intake, control plane, dispatch, scheduling, channels, policy, and all core writes. Python for batch analytics, metering export, anomaly detection, AI-assisted features, and engineering tooling. Contracts via OpenAPI/proto. Python uses a read-only DB role and writes only to its own schemas.
**Consequences.** (+) Failure isolation, right tool per job. (−) Two toolchains to maintain; mitigated by generated clients, shared CI patterns, and keeping Python off the critical path. Any proposal to move core logic into Python or analytics into the Go hot path needs a new ADR.
