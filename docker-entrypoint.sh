#!/bin/sh
#
# Orbitron container entrypoint.
#
# Renders /etc/orbitron/config.yml from ORBITRON_* environment variables,
# optionally bootstraps an administrative token on first start, and then execs
# the daemon as the non-root "orbitron" user so container signals reach the
# daemon directly (PID 1) and terminate it gracefully.
#
# Every variable is optional; the defaults mirror the daemon's built-in
# defaults (see internal/config/config.go).

set -eu

: "${ORBITRON_LISTEN_ADDR:=0.0.0.0:8080}"
: "${ORBITRON_STORAGE_PATH:=/data}"
: "${ORBITRON_LOG_PATH:=}"
: "${ORBITRON_TOKENS_FILE:=${ORBITRON_STORAGE_PATH}/tokens.json}"
: "${ORBITRON_REQUIRE_AUTH_PULL:=false}"
: "${ORBITRON_MAX_CONCURRENCY:=4}"
: "${ORBITRON_TOKEN_TTL_DAYS:=0}"
: "${ORBITRON_HTTP_PROXY:=}"
: "${ORBITRON_HTTPS_PROXY:=}"
: "${ORBITRON_NO_PROXY:=}"
: "${ORBITRON_AUTO_TOKEN:=true}"
: "${ORBITRON_CONFIG_FILE:=/etc/orbitron/config.yml}"

CONF_DIR="$(dirname "${ORBITRON_CONFIG_FILE}")"
STORAGE_DIR="${ORBITRON_STORAGE_PATH}"
TOKENS_DIR="$(dirname "${ORBITRON_TOKENS_FILE}")"

mkdir -p "${CONF_DIR}" "${STORAGE_DIR}/roles" "${STORAGE_DIR}/collections" \
    "${STORAGE_DIR}/manifests" "${TOKENS_DIR}"
chown -R orbitron:orbitron "${STORAGE_DIR}" "${TOKENS_DIR}"

cat > "${ORBITRON_CONFIG_FILE}" <<EOF
listen_addr: "${ORBITRON_LISTEN_ADDR}"
storage_path: "${ORBITRON_STORAGE_PATH}"
log_path: "${ORBITRON_LOG_PATH}"
tokens_file: "${ORBITRON_TOKENS_FILE}"
require_auth_pull: ${ORBITRON_REQUIRE_AUTH_PULL}
max_concurrency: ${ORBITRON_MAX_CONCURRENCY}
token_ttl_days: ${ORBITRON_TOKEN_TTL_DAYS}
http_proxy: "${ORBITRON_HTTP_PROXY}"
https_proxy: "${ORBITRON_HTTPS_PROXY}"
no_proxy: "${ORBITRON_NO_PROXY}"
EOF
chown root:orbitron "${ORBITRON_CONFIG_FILE}"
chmod 0640 "${ORBITRON_CONFIG_FILE}"

if [ "${ORBITRON_AUTO_TOKEN}" = "true" ] && \
    [ ! -s "${ORBITRON_TOKENS_FILE}" ]; then
    echo "[ENTRYPOINT] No admin token found; generating a fresh one..."
    TOKEN="$(su-exec orbitron /usr/local/bin/orbitron -config "${ORBITRON_CONFIG_FILE}" -q -generate-token)"
    echo "[ENTRYPOINT] ADMIN TOKEN: ${TOKEN}"
    echo "[ENTRYPOINT] Store this token in your Ansible client configuration and keep it safe."
fi

echo "[ENTRYPOINT] Starting Orbitron on ${ORBITRON_LISTEN_ADDR} (storage: ${ORBITRON_STORAGE_PATH})"
exec su-exec orbitron /usr/local/bin/orbitron -config "${ORBITRON_CONFIG_FILE}" "$@"
