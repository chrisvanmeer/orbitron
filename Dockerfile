# -- Stage 1: Build the static binary --
FROM golang:1.26-alpine AS builder

ARG VERSION=dev

WORKDIR /src

# Copy dependency manifests first to leverage Docker layer caching
COPY go.mod go.sum* ./
RUN go mod download

# Copy the remaining source code
COPY . .

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
