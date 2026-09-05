# syntax=docker/dockerfile:1
#
# Container image for the PoolPilot Relay, shipped as a Home Assistant app
# (see ../../poolpilot_relay/). The Home Assistant Supervisor pulls this
# pre-built image via the `image:` key in poolpilot_relay/config.yaml — it does
# NOT build it locally, so the build context here is the REPOSITORY ROOT (the
# Go module), driven by .github/workflows/addon-release.yml:
#
#     docker buildx build -f deploy/docker/Dockerfile --build-arg VERSION=... .
#
# The builder stage cross-compiles with Go's native cross-compiler on the build
# platform (no QEMU emulation), so multi-arch builds stay fast.

FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
RUN apk add --no-cache ca-certificates tzdata
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG TARGETOS TARGETARCH TARGETVARIANT
RUN CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
      GOARM="$(printf '%s' "${TARGETVARIANT}" | tr -d 'v')" \
      go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/poolpilot-relay ./cmd/poolpilot-relay

FROM scratch
LABEL org.opencontainers.image.source="https://github.com/ylabonte/poolpilot-relay" \
      org.opencontainers.image.title="PoolPilot Relay" \
      org.opencontainers.image.description="PoolPilot edge agent, packaged as a Home Assistant app" \
      org.opencontainers.image.licenses="MIT"

# TLS trust store (the relay talks HTTPS to the PoolPilot cloud and to the
# controllers) and zoneinfo, plus the license texts we redistribute.
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /usr/share/zoneinfo /usr/share/zoneinfo
COPY LICENSE NOTICE /
COPY --from=build /out/poolpilot-relay /usr/local/bin/poolpilot-relay

# Fixed configuration for the app runtime:
#   CLOUD_BASE_URL  hard-wired to our control plane (never user-configurable).
#   STATE_PATH      the app's persistent /data volume — this holds the pairing
#                   AND the TLS identity whose SPKI fingerprint every paired app
#                   pins, so it MUST survive restarts/reinstalls.
#   UPDATE_DISABLED the Home Assistant Supervisor owns the update lifecycle; the
#                   relay must not swap its own binary inside the container.
ENV CLOUD_BASE_URL=https://api.poolpilot.eu \
    STATE_PATH=/data/state.json \
    UPDATE_DISABLED=1 \
    TZ=UTC

ENTRYPOINT ["/usr/local/bin/poolpilot-relay"]
