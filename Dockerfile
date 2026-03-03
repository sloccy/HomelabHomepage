# syntax=docker/dockerfile:1
# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.22-bookworm AS builder

# Build-time metadata (injected by CI via --build-arg).
ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown

WORKDIR /build

# Restore modules before copying source to maximise layer cache reuse.
COPY go.mod go.sum ./
# --mount=type=cache keeps the module cache between BuildKit runs,
# so repeated builds don't re-download the internet.
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download -x

# Copy source and compile.
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${BUILD_VERSION} -X main.commit=${BUILD_COMMIT}" \
      -o launchpad .

# ── Stage 2: final ────────────────────────────────────────────────────────────
FROM gcr.io/distroless/static-debian12:nonroot

# CA certificates for outbound HTTPS (ipify, Cloudflare, ACME).
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Binary.
COPY --from=builder /build/launchpad /launchpad

# OCI image labels (populated by docker/metadata-action in CI).
ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
LABEL org.opencontainers.image.title="Launchpad" \
      org.opencontainers.image.description="Homelab reverse proxy, service discovery & homepage" \
      org.opencontainers.image.version="${BUILD_VERSION}" \
      org.opencontainers.image.revision="${BUILD_COMMIT}" \
      org.opencontainers.image.source="https://github.com/brendan/HomelabHomepage"

# Ports: 80 (HTTP → HTTPS redirect), 443 (HTTPS proxy + GUI).
# NET_BIND_SERVICE capability required when running as nonroot — see docker-compose.yml.
EXPOSE 80 443

ENTRYPOINT ["/launchpad"]
