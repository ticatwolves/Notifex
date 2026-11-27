# ADR-003: Next.js console is a separate deployable on Fargate behind CloudFront
Status: Accepted

**Decision.** Frontend and backend deploy independently with a generated-client contract. Console runs as a standalone Next.js container on Fargate (0.25–0.5 vCPU), static assets cached via CloudFront. Server components call the Go API; the Go API is the sole authorization authority.
**Consequences.** (+) Same ops model as backend, simple rollbacks, no vendor lock-in. (−) Pays for an always-on task; if the console cost matters more than uniformity, move to OpenNext/Lambda or Amplify later.
