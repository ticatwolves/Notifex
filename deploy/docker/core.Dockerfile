# syntax=docker/dockerfile:1.7
# Notifex Go core: one image, several entrypoints (api, dispatcher, scheduler,
# migrate). ECS task definitions and compose override the command per service.
# Build context is the repo root. Base images are pinned by digest; bump them
# deliberately (Dependabot/Renovate) and re-run the image scan.

FROM --platform=$BUILDPLATFORM golang:1.27@sha256:5f1b98fa02fe396ebce2a0227a0e08309e11792915b9dd2e1f71053d7e7e6666 AS build
ARG TARGETOS TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY ent ./ent
COPY migrations ./migrations
# Cross-compile natively on the build host; the runtime stage has no RUN steps,
# so building linux/arm64 on an amd64 runner needs no QEMU.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    for svc in api dispatcher scheduler migrate healthcheck; do \
      CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
        -ldflags="-s -w -X github.com/sbshobhit/notifex/internal/app.Version=${VERSION}" \
        -o /out/$svc ./cmd/$svc || exit 1; \
    done

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/ /app/
USER nonroot:nonroot
EXPOSE 8080 9090 9100
# CMD, not ENTRYPOINT: ECS task definitions and compose replace `command` with
# the binary to run (/app/dispatcher, /app/migrate up, ...). With an ENTRYPOINT
# that command would be passed to /app/api as arguments and silently ignored.
CMD ["/app/api"]
