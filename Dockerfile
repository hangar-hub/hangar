# syntax=docker/dockerfile:1
#
# Hangar container image (docs/spec/12-packaging-and-release.md).
#
#   docker build -t hangar .
#   docker buildx build --platform linux/amd64,linux/arm64 -t hangar .
#
# The UI and the binary are built on the build platform and cross-compiled,
# so multi-arch builds need no emulation except for the small root
# filesystem stage.

ARG GO_VERSION=1.27.2
ARG NODE_VERSION=24.19.0

# 1. The web UI, built into internal/web/dist.
FROM --platform=$BUILDPLATFORM node:${NODE_VERSION}-alpine AS ui
WORKDIR /src/ui
RUN corepack enable
COPY ui/package.json ui/pnpm-lock.yaml ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store \
    pnpm install --frozen-lockfile
COPY ui/ ./
RUN mkdir -p ../internal/web/dist && pnpm run build

# 2. A static hangar binary for the target platform.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
COPY --from=ui /src/internal/web/dist ./internal/web/dist
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}" \
      -o /out/hangar ./cmd/hangar

# 3. A minimal Wolfi root filesystem: the base layout, CA certificates, time
# zones and git (the server runs git upload-pack), and nothing else, so no
# shell or package manager. /etc/hangar/hangar.yaml sets no keys, so the
# image reads it without changing a default; mount your own file over it.
FROM cgr.dev/chainguard/wolfi-base:latest AS rootfs
RUN apk add --no-cache --initdb --root /rootfs \
      --keys-dir /etc/apk/keys --repositories-file /etc/apk/repositories \
      wolfi-baselayout ca-certificates-bundle tzdata git \
 && rm -rf /rootfs/var/cache/apk /rootfs/lib/apk /rootfs/etc/apk \
 && mkdir -p /rootfs/var/lib/hangar /rootfs/var/cache/hangar /rootfs/etc/hangar /rootfs/tmp \
 && printf '%s\n' \
      '# Hangar configuration. This file sets nothing, so every key has its default.' \
      '# Mount your own file here, or set HANGAR_* environment variables, which' \
      '# override it. Every key: https://jamesagarside.github.io/hangar/reference/configuration/' \
      > /rootfs/etc/hangar/hangar.yaml \
 && chmod 0644 /rootfs/etc/hangar/hangar.yaml \
 && chown -R 65532:65532 /rootfs/var/lib/hangar /rootfs/var/cache/hangar \
 && chmod 1777 /rootfs/tmp

# 4. The image.
FROM scratch
ARG VERSION=dev
ARG COMMIT=none
COPY --from=rootfs /rootfs/ /
COPY --from=build /out/hangar /usr/bin/hangar
LABEL org.opencontainers.image.title="Hangar" \
      org.opencontainers.image.description="A self-hosted home for your arsenal of models: a Hugging Face Hub-compatible model and dataset hub." \
      org.opencontainers.image.source="https://github.com/jamesagarside/hangar" \
      org.opencontainers.image.documentation="https://jamesagarside.github.io/hangar/" \
      org.opencontainers.image.licenses="Elastic-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}"
# The defaults keep data in /var/lib/hangar and the git pack cache in
# /var/cache/hangar. Hangar reads its configuration file from
# /etc/hangar/hangar.yaml (optional: environment variables are enough), and
# --config on a command line wins over HANGAR_CONFIG.
ENV HOME=/var/lib/hangar \
    TMPDIR=/tmp \
    HANGAR_CONFIG=/etc/hangar/hangar.yaml
USER 65532:65532
WORKDIR /var/lib/hangar
VOLUME ["/var/lib/hangar"]
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=5s --start-period=20s --retries=3 CMD ["hangar", "healthcheck"]
ENTRYPOINT ["hangar"]
CMD ["server"]
