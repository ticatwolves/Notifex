# ADR-001: Run on AWS ECS Fargate (ARM64) with Terraform, not EKS or Lambda-only
Status: Accepted (revisit at Stage C)

**Context.** Solo/small team, need low ops burden and low starting cost with a clear path to scale. Workloads: API (REST+gRPC), long-running workers, scheduled jobs, web app.
**Decision.** ECS on Fargate (Graviton), Spot for interruption-tolerant workers, ALB with gRPC target group, Service Connect for internal traffic, Terraform for all infra, GitHub Actions with OIDC.
**Consequences.** (+) No node management, per-second billing, ~20% ARM saving, Spot savings for workers, simple deploy model. (−) Less ecosystem than Kubernetes; Fargate unit price above well-packed EC2. Revisit EKS only for service mesh/operator needs; revisit EC2 capacity/Savings Plans for steady high volume.
**Alternatives.** EKS ($73/mo control plane + ops), Lambda-only (cold starts, DB connections, gRPC friction), App Runner (less control), EC2 + compose (no HA path).
