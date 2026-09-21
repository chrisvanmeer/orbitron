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
# Optional TLS policy for outbound HTTPS (Galaxy API/downloads, git HTTPS
# remotes, OIDC discovery). ORBITRON_TLS_CA_FILE points at a PEM bundle that
# signs internal services (mounted into the container); set
# ORBITRON_TLS_INSECURE_SKIP_TLS_VERIFY=true to skip verification entirely.
: "${ORBITRON_TLS_CA_FILE:=}"
: "${ORBITRON_TLS_INSECURE_SKIP_TLS_VERIFY:=false}"
: "${ORBITRON_AUTO_TOKEN:=true}"
: "${ORBITRON_CONFIG_FILE:=/etc/orbitron/config.yml}"

# Optional OpenID Connect (SSO) authentication for the web dashboard. When
# enabled the /ui login page gains a "Sign in with SSO" button in addition to
# the regular access-token login.
: "${ORBITRON_OIDC_ENABLED:=false}"
: "${ORBITRON_OIDC_ISSUER:=}"
: "${ORBITRON_OIDC_CLIENT_ID:=}"
: "${ORBITRON_OIDC_CLIENT_SECRET:=}"
: "${ORBITRON_OIDC_SESSION_TTL_HOURS:=8}"
: "${ORBITRON_OIDC_REDIRECT_URI:=}"
# Comma-separated list of Keycloak group names; only members of at least one
# listed group may log in. Leave empty to admit every verified SSO user. The ID
# token must carry a "groups" claim (Keycloak: add the "groups" client scope /
# a "Group Membership" mapper with "Add to ID token: ON").
: "${ORBITRON_OIDC_ALLOWED_GROUPS:=}"

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

# Append the TLS policy only when a CA bundle is configured or verification
# is skipped, keeping the default configuration deterministic.
if [ -n "${ORBITRON_TLS_CA_FILE}" ] || \
    [ "${ORBITRON_TLS_INSECURE_SKIP_TLS_VERIFY}" = "true" ]; then
    cat >> "${ORBITRON_CONFIG_FILE}" <<EOF
tls:
  ca_file: "${ORBITRON_TLS_CA_FILE}"
  insecure_skip_tls_verify: ${ORBITRON_TLS_INSECURE_SKIP_TLS_VERIFY}
EOF
fi

# Append the optional OIDC block only when SSO is enabled so a disabled
# configuration stays deterministic.
if [ "${ORBITRON_OIDC_ENABLED}" = "true" ]; then
    # Render the optional group filter as a YAML inline list from the
    # comma-separated env var.
    ALLOWED_GROUPS_YAML=
    if [ -n "${ORBITRON_OIDC_ALLOWED_GROUPS}" ]; then
        ALLOWED_GROUPS_YAML="  allowed_groups: [${ORBITRON_OIDC_ALLOWED_GROUPS}]"
    fi
    cat >> "${ORBITRON_CONFIG_FILE}" <<EOF
oidc:
  enabled: true
  issuer: "${ORBITRON_OIDC_ISSUER}"
  client_id: "${ORBITRON_OIDC_CLIENT_ID}"
  client_secret: "${ORBITRON_OIDC_CLIENT_SECRET}"
  session_ttl_hours: ${ORBITRON_OIDC_SESSION_TTL_HOURS}
  redirect_uri: "${ORBITRON_OIDC_REDIRECT_URI}"
${ALLOWED_GROUPS_YAML}
EOF
fi
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
