#!/bin/sh
#
# Orbitron container health check.
#
# Probes the daemon's unauthenticated /healthz endpoint. The port is derived
# from ORBITRON_LISTEN_ADDR at runtime so the check keeps working when the
# container is started on a custom listen address.

set -eu

LISTEN="${ORBITRON_LISTEN_ADDR:-0.0.0.0:8080}"
PORT="${LISTEN##*:}"

case "${PORT}" in
    *[!0-9]* | '')
        echo "[HEALTHCHECK] cannot derive a port from ORBITRON_LISTEN_ADDR=${LISTEN}" >&2
        exit 1
        ;;
esac

wget -qO- "http://127.0.0.1:${PORT}/healthz" >/dev/null || exit 1