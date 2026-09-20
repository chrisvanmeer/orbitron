# -- Stage 0: Pre-populate the Go build cache --------------------------
# Compiles the same package closure as the real build (same CGO/trimpath
# flags, no version stamp) so /root/.cache/go-build is warm. This stage's
# layers only invalidate when source actually changes, and are persisted in
# CI via the GHA build cache (mode=max), so tagging a release or updating
# docs re-runs the final `go build` in seconds instead of minutes.
FROM golang:1.26-alpine AS buildcache

WORKDIR /src

# Copy dependency manifests first to leverage Docker layer caching.
COPY go.mod go.sum* ./
RUN go mod download

COPY main.go ./main.go
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /dev/null .

# -- Stage 1: Build the static binary --
FROM golang:1.26-alpine AS builder

ARG VERSION=dev

WORKDIR /src

# Reuse the warm Go build and module caches instead of compiling on a cold
# cache for every version stamp / docs commit.
COPY --from=buildcache /root/.cache/go-build /root/.cache/go-build
COPY --from=buildcache /go/pkg/mod /go/pkg/mod

# Copy dependency manifests first to leverage Docker layer caching.
COPY go.mod go.sum* ./
RUN go mod download

# Copy the remaining source code.
COPY main.go ./main.go
COPY internal ./internal

# Build the static binary, stamping the release version so
# `orbitron --version` reports e.g. "orbitron v1.4.1".
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-w -s -X orbitron/internal/build.Version=${VERSION}" \
    -o /orbitron .

# -- Stage 2: Minimal runtime image --
FROM alpine:3.21

# git + openssh-client are required to mirror git-backed roles and collections.
# ca-certificates for HTTPS Galaxy API calls, tzdata for timezone-aware logs,
# su-exec to drop from the entrypoint (root) to the non-root daemon user.
RUN apk add --no-cache ca-certificates git openssh-client tzdata su-exec

# Create a non-root user that owns the on-disk cache.
RUN adduser -D -H -g 'orbitron daemon' -s /sbin/nologin orbitron

COPY --from=builder /orbitron /usr/local/bin/orbitron
COPY docker-entrypoint.sh /docker-entrypoint.sh
COPY docker-healthcheck.sh /usr/local/bin/orbitron-healthcheck

RUN chmod 0755 /docker-entrypoint.sh /usr/local/bin/orbitron-healthcheck && \
    mkdir -p /data && chown orbitron:orbitron /data

USER root

EXPOSE 8080

VOLUME ["/data"]

ENTRYPOINT ["/docker-entrypoint.sh"]

# The port is taken from ORBITRON_LISTEN_ADDR at runtime so the check follows
# a custom listen address (see docker-healthcheck.sh).
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
    CMD /usr/local/bin/orbitron-healthcheck
