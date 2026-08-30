# syntax=docker/dockerfile:1

# yt-dlp-manager — three-stage, multi-architecture Alpine image.
# Build:
#   docker build -t yt-dlp-manager .
#
# The image contains: the statically compiled manager binary, a pinned
# checksum-verified official yt-dlp zipapp, Alpine's ffmpeg/ffprobe, and
# QuickJS as the JavaScript engine yt-dlp uses for YouTube extraction.

# Single source of truth for the pinned yt-dlp release. Bump it here and the
# Makefile, Compose file and CI all follow — none of them repeat the number.
ARG YT_DLP_VERSION=2026.08.19

# Alpine's tag is still an ARG because an OCI label below reports it. The FROM
# lines are deliberately NOT parameterised: Dependabot's docker updater can only
# read a literal reference, so `FROM alpine:${ALPINE_VERSION}` made every base
# image invisible to it — the repository ran a docker updater for weeks and it
# never once proposed a bump. Keep this value in step with the alpine FROM line.
ARG ALPINE_VERSION=3.24

# --- Stage 1: frontend builder -------------------------------------------
# Pinned to $BUILDPLATFORM, so this stage always runs natively even when the
# image being produced is for another architecture. Two reasons:
#
#   1. Its output is JavaScript and CSS. Nothing here is architecture-specific,
#      so emulating it buys nothing — the bytes handed to COPY are identical.
#   2. Node does not survive qemu-user arm64. `npm ci` dies with "uncaught
#      target signal 4 (Illegal instruction)" when V8 reaches an instruction
#      QEMU does not implement, then retries until the job is measured in
#      hours. Everything else in this file (Alpine, wget, the Go toolchain)
#      emulates fine; this stage alone is why cross-builds were unusable.
#
# The cost is that $BUILDPLATFORM needs BuildKit, so this file no longer builds
# under a pre-23 Docker without buildx. BuildKit has been the default since
# Docker 23, and CI drives it through buildx, so that is a safe trade — but it
# IS a narrowing of what used to be promised here.
#
# Release builds now run each architecture on its own native runner (see the
# image matrix in .github/workflows/release.yml), so nothing is emulated there
# and $BUILDPLATFORM simply equals the target. This pinning still matters for
# anyone cross-building by hand — `docker buildx build --platform linux/arm64`
# on an amd64 machine — which is exactly the case QEMU breaks.
FROM --platform=$BUILDPLATFORM node:26-alpine@sha256:2d984a15c9b54fd0aeb608b8e0d0d83529eb34d2966db27a1fb4f1edc3d298a3 AS frontend
WORKDIR /build
COPY internal/webui/frontend/package.json internal/webui/frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY internal/webui/frontend/ ./
RUN npm run typecheck && npm test && npm run build

# --- Stage 2: Go builder ---------------------------------------------------
FROM golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS gobuild
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
# Use the freshly built frontend instead of the committed snapshot.
COPY --from=frontend /static ./internal/webui/static
ARG APP_VERSION=dev
ARG APP_COMMIT=unknown
ARG APP_DATE=unknown
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w \
      -X yt-dlp-manager/internal/app.Version=${APP_VERSION} \
      -X yt-dlp-manager/internal/app.Commit=${APP_COMMIT} \
      -X yt-dlp-manager/internal/app.Date=${APP_DATE}" \
    -o /out/yt-dlp-manager ./cmd/yt-dlp-manager

# --- Stage 3: runtime -------------------------------------------------------
FROM alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS runtime
ARG YT_DLP_VERSION
ARG ALPINE_VERSION
ARG APP_VERSION=dev
ENV HOME=/tmp/home \
    XDG_CONFIG_HOME=/config \
    XDG_STATE_HOME=/state \
    XDG_CACHE_HOME=/tmp/cache \
    XDG_RUNTIME_DIR=/tmp/runtime

LABEL org.opencontainers.image.title="yt-dlp-manager" \
      org.opencontainers.image.description="Self-hosted yt-dlp download manager (web UI + TUI + CLI)" \
      org.opencontainers.image.source="https://github.com/ih8d8/yt-dlp-manager" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${APP_VERSION:-dev}" \
      org.opencontainers.image.vendor.yt_dlp="${YT_DLP_VERSION}" \
      org.opencontainers.image.vendor.alpine="${ALPINE_VERSION}"

# Runtime packages only: certs, timezone data, ffmpeg (+ffprobe), python3
# (runs the yt-dlp zipapp), QuickJS (the JavaScript engine yt-dlp uses to solve
# YouTube's player challenges), and su-exec for the entrypoint privilege drop.
#
# Why QuickJS rather than Deno or Node: yt-dlp supports deno, node, quickjs and
# bun, and all four solve the same challenges — but Deno is 89 MB and Node is
# 53 MB, while QuickJS is under 2 MB. Dropping both in favour of qjs removes
# ~140 MB from the image for the same capability, verified by running a real
# YouTube extraction against it. yt-dlp enables ONLY deno by default, so the
# system config below is what makes qjs the runtime it actually reaches for.
#
# If a future yt-dlp ever needs a Deno-specific feature, rebuild with
#   docker build --build-arg JS_RUNTIME=deno .
# (or JS_RUNTIME=node). Nothing else changes; see the case statement below.
ARG JS_RUNTIME=quickjs
RUN case "$JS_RUNTIME" in \
      quickjs) JS_PKG=quickjs ;; \
      deno)    JS_PKG=deno ;; \
      node)    JS_PKG=nodejs ;; \
      *) echo "JS_RUNTIME must be quickjs, deno or node (got: $JS_RUNTIME)" >&2; exit 1 ;; \
    esac \
    # A digest-pinned Alpine base is reproducible but its installed packages
    # can lag security fixes already published to the same stable branch. Apply
    # those fixes before adding the runtime packages; Trivy then gates the
    # resulting image in CI and release verification.
 && apk upgrade --no-cache \
 && apk add --no-cache ca-certificates tzdata ffmpeg python3 su-exec "$JS_PKG" \
    && addgroup -S yt-dlp-manager \
    && adduser -S -G yt-dlp-manager -h "$HOME" -s /sbin/nologin yt-dlp-manager \
    && mkdir -p /downloads /config /state /tmp/runtime \
    && chown -R yt-dlp-manager:yt-dlp-manager /config /downloads /state /tmp/runtime "$HOME" \
    && chmod 700 /tmp/runtime \
    && rm -rf /usr/share/man /usr/share/doc /usr/share/info

# System-level yt-dlp config. yt-dlp reads this BEFORE the user config, and the
# user config in /config/yt-dlp/config still wins on anything it sets — so this
# only supplies the default runtime and never overrides your choices.
ARG JS_RUNTIME=quickjs
RUN printf -- '# Managed by the yt-dlp-manager container image.\n\
# yt-dlp enables only "deno" by default, so whichever runtime this image ships\n\
# has to be named explicitly. Your own config still takes precedence.\n\
--js-runtimes %s\n' "$JS_RUNTIME" > /etc/yt-dlp.conf

# Pinned official yt-dlp standalone release asset (the self-contained zipapp),
# verified against the release's own SHA2-256SUMS manifest at build time.
#
# Why the zipapp instead of the musllinux PyInstaller binary: live testing
# showed that build is broken on current Alpine — its bootloader fails to
# relocate against zlib-ng ("inflateEnd: symbol not found") and it must
# self-extract ~100 MiB into /tmp on every single download invocation. The
# zipapp has neither problem and is the same pinned upstream artifact. The
# zipapp is architecture-independent. Nothing is downloaded at runtime and
# `yt-dlp -U` inside the container is unsupported by design.
RUN set -eu \
    && [ -n "${YT_DLP_VERSION}" ] || { echo "YT_DLP_VERSION must not be empty (see the ARG at the top of this file)" >&2; exit 1; } \
    && base="https://github.com/yt-dlp/yt-dlp/releases/download/${YT_DLP_VERSION}" \
    && wget -qO /tmp/yt-dlp "${base}/yt-dlp" \
    && wget -qO /tmp/SHA2-256SUMS "${base}/SHA2-256SUMS" \
    && grep "  yt-dlp\$" /tmp/SHA2-256SUMS > /tmp/yt-dlp.sha256 \
    && test "$(wc -l < /tmp/yt-dlp.sha256)" = "1" \
    && cd /tmp && sha256sum -c yt-dlp.sha256 && cd / \
    && install -m 0755 /tmp/yt-dlp /usr/local/bin/yt-dlp \
    && rm -rf /tmp/*

COPY --from=gobuild /out/yt-dlp-manager /usr/local/bin/yt-dlp-manager
COPY docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh
RUN chmod 0755 /usr/local/bin/docker-entrypoint.sh

# The entrypoint starts with the container's deliberately narrow capability
# set, repairs only app-owned paths, then immediately drops to PUID:PGID.
USER root
# Default the process working directory to the media volume so that, out of
# the box, yt-dlp's default relative output lands somewhere writable. This
# changes no argv and no yt-dlp option — your configuration stays fully
# authoritative (--paths/--output always win).
WORKDIR /downloads
EXPOSE 8080
VOLUME ["/downloads", "/config", "/state"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["yt-dlp-manager", "healthcheck"]

# Starts as root only to run the scoped ownership bootstrap in
# docker-entrypoint.sh (never recursively touches media), then execs the
# server as the configured non-root host UID/GID.
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["server"]
