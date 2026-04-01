# syntax=docker/dockerfile:1
# ── Stage 1: build ────────────────────────────────────────────────────────────
FROM golang:1.26-trixie AS builder

# Build-time metadata (injected by CI via --build-arg).
ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
# Populated automatically by BuildKit when --platform is specified.
ARG TARGETARCH

WORKDIR /build

# Create the data directory that will be copied to the final image with correct ownership.
RUN mkdir -p /data

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
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} \
    go build -trimpath \
      -ldflags="-s -w -X main.version=${BUILD_VERSION} -X main.commit=${BUILD_COMMIT}" \
      -o lantern .

# Grant the binary privileged capabilities as a non-root user:
#   cap_net_bind_service — bind to ports 80/443
#   cap_net_raw          — raw sockets for ARP pre-sweep (faster network scanning)
# File capabilities are preserved by COPY into the final image.
RUN apt-get update -qq && \
    apt-get install -y --no-install-recommends libcap2-bin curl && \
    setcap 'cap_net_bind_service=+ep cap_net_raw=+ep' /build/lantern

# Download cloudflared for tunnel management.
RUN curl -fsSL -o /cloudflared \
    "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-linux-${TARGETARCH}" \
    && chmod +x /cloudflared

# ── Stage 2: final ────────────────────────────────────────────────────────────
# glibc-dynamic is required for the dynamically-linked cloudflared binary.
# Chainguard images are rebuilt daily with security patches (lower CVE count than distroless).
FROM cgr.dev/chainguard/glibc-dynamic:latest

# CA certificates are bundled in Chainguard images — no manual copy needed.

# Binary.
COPY --from=builder /build/lantern /lantern
COPY --from=builder /cloudflared /cloudflared

# Pre-create data directory owned by the nonroot user (UID 65532).
# This ensures Docker initialises the named volume with correct ownership on first run.
COPY --from=builder --chown=65532:65532 /data /data

# OCI image labels (populated by docker/metadata-action in CI).
ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
LABEL org.opencontainers.image.title="Lantern" \
      org.opencontainers.image.description="Homelab reverse proxy, service discovery & homepage" \
      org.opencontainers.image.version="${BUILD_VERSION}" \
      org.opencontainers.image.revision="${BUILD_COMMIT}" \
      org.opencontainers.image.source="" \
      org.opencontainers.image.licenses="MIT"

# Ports: 80 (HTTP → HTTPS redirect), 443 (HTTPS proxy + GUI).
# Privileged port binding is handled by the file capability set on the binary above (setcap).
EXPOSE 80 443

# Health check: distroless has no shell, so the binary handles its own probe.
# JSON exec form avoids any /bin/sh dependency. docker-compose.yml must NOT
# override this with a YAML healthcheck block or it will reintroduce the issue.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD ["/lantern", "healthcheck"]

ENTRYPOINT ["/lantern"]
