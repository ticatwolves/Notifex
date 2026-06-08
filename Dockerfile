# ── Stage 1: dependency cache ─────────────────────────────────────────
FROM golang:1.26.4-alpine AS deps
WORKDIR /app
# Copy only mod files first — layer cache survives code changes
COPY go.mod go.sum ./
RUN go mod download

# ── Stage 2: build ────────────────────────────────────────────────────
FROM deps AS builder
COPY . .

# Build static binary — no CGO, no external dependencies
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build \
    -ldflags="-w -s -X main.version=${VERSION} -X main.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    -trimpath \
    -o /app/api \
    ./cmd/api

# ── Stage 3: security scan ────────────────────────────────────────────
FROM aquasec/trivy:latest AS scanner
COPY --from=builder /app/api /app/api
RUN trivy fs --exit-code 1 --severity HIGH,CRITICAL /app/api

# ── Stage 4: production image ─────────────────────────────────────────
FROM scratch AS production
# scratch = empty image, zero OS attack surface

# TLS certificates (needed for outbound HTTPS calls) - Add if app makes outbound HTTPS calls and you want to include CA certs
# COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Timezone data (needed if app uses timezones) - Add if app uses timezones and you want to include tzdata
# COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

COPY --from=builder /etc/passwd /etc/passwd
COPY --from=builder /app/api /api

USER nobody
EXPOSE 5000
ENTRYPOINT ["/api"]
