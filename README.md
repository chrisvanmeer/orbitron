<div align="center">
  <img src="assets/logo.svg" alt="Orbitron Logo" width="100%">
</div>

# 🌌 Orbitron

A custom, lightweight Ansible Galaxy and Git-based role/collection mirror daemon written in Go.

Orbitron acts as a local mirror for Ansible Galaxy content and Git repositories, allowing isolated networks or
enterprise environments to cache, store, and serve Ansible roles and collections directly to `ansible-galaxy` clients.

---

## Key Features

* **Galaxy V1 & V3 API Support**: Complete compatibility with `ansible-galaxy role install` and
  `ansible-galaxy collection install`.
* **Cyberpunk Web Dashboard (`/ui`)**: Full-screen, zero-dependency terminal UI featuring cookie-backed session auth,
  live log streaming, and system telemetry.
* **On-the-Fly Archiving**: Packages unpacked Git-based roles into `.tar.gz` streams on-the-fly during download.
* **Parallel Background Sync**: Concurrent fetching of roles, collections, and shallow Git clones (`--depth 1`).
* **Flexible Authentication**: Unauthenticated pulls by default with an optional `require_auth_pull: true` toggle
  supporting Bearer tokens and HTTP Basic Auth.
* **Automated Lifecycle Management**: Integrated CLI commands for user creation (`orbitron:orbitron`),
  systemd service registration, logrotate configuration, and full uninstallation cleanup.
* **Storage Pruner**: Built-in CLI flag (`--prune`) to scan manifests, detect orphaned versions, and clean up
  disk usage.
* **Liveness Probe (`/healthz`)**: Unauthenticated health endpoint for orchestrators, load balancers, and uptime
  monitors; reports `200` when the storage path is writable and `503` otherwise.
* **Async Sync Status API**: `GET /api/v1/sync/status` exposes the currently running sync job (kind, progress,
  failures) plus a bounded history of recent syncs.
* **Token Lifecycle API**: Create, list, revoke, and rotate administrative tokens over HTTP, with native expiry
  support and `token_ttl_days` configuration.
* **Content Inventory APIs**: `GET /api/v1/manifests` lists the stored requirement manifests (with content
  hashes) and `GET /api/v1/storage` exposes the precise cached-version inventory of roles and collections.
* **Official Ansible Collection (`chrisvanmeer.orbitron`)**: install, configure, and operate the daemon purely
  with Ansible – six purpose-built HTTP modules plus declarative install/mirror roles.

---

## 🖥️ Cyberpunk Web Dashboard (`/ui`)

Orbitron includes a full-screen Cyberpunk-themed Web UI hosted at `/ui` for real-time monitoring and storage inspection.

<div align="center">
  <img src="assets/ui.png" alt="Orbitron Cyberpunk Web UI" width="100%">
</div>

### Dashboard Features

* **Local Cache Matrix**: Fullscreen view of all cached roles and collections. Actively declared versions from
  ingested manifests are highlighted with an `[ACTIVE]` tag alongside precise physical block-level disk usage.
* **Cookie-Based Authentication**: Secure login modal backed by an HTTP-only 8-hour cookie session using any
  valid administrative token.
* **Collapsible Log Drawer**: Bottom sliding drawer (`▲ LOG STREAM`) providing a live feed of
  `/var/log/orbitron/orbitron.log` (automatically filtered to suppress HTTP polling noise).
* **Collapsible System Metrics Drawer**: Right sliding sidebar (`◄ SYS METRICS`) displaying uplink sync status,
  last manifest ingest timestamp, cache disk usage, mount free space, normalized OS distribution/version,
  system architecture (e.g., `AMD64`, `ARM64`), and last boot time.
* **Air-Gapped / Island-Mode Ready**: Embedded HTMX 4.0.0 served directly from memory, eliminating external
  CDN calls or outbound network dependencies.

---

## Installation & Service Management

### Building from Source

```bash
make build
```

### Pre-compiled binaries

With each version increment, set of pre-compiled binaries are generated and available for direct download:
<https://github.com/chrisvanmeer/orbitron/releases>

Self contained binaries are provided for Linux AMD64 and Linux ARM64 architectures.

### Installing Orbitron

Executing `--install` as root automatically creates the `orbitron` system user/group, directories,
configuration files, systemd unit, and logrotate script, then enables and starts the daemon:

```bash
sudo ./bin/orbitron --install
```

### Daemon Service Control

```bash
sudo systemctl status orbitron
sudo systemctl restart orbitron
sudo systemctl stop orbitron
```

### Full Uninstallation

Executing `--uninstall` stops and disables the systemd unit, cleans up storage paths and manifests,
and removes system users and binaries:

```bash
sudo ./bin/orbitron --uninstall
```

---

## CLI Usage & Token Management

```bash
# Generate an administrative Bearer token
sudo orbitron --generate-token

# Generate raw token output (quiet mode for script exports)
export ORBITRON_TOKEN=$(sudo orbitron --generate-token -q)

# Revoke a token
sudo orbitron --revoke-token <TOKEN>

# Interactively scan and prune unreferenced role/collection versions
sudo orbitron --prune

# Start daemon with custom config
orbitron --config /etc/orbitron/config.yml

# Show the compiled-in version
orbitron --version
```

| Flag               | Shorthand | Description                                                             |
| :----------------- | :-------- | :---------------------------------------------------------------------- |
| `--generate-token` |           | Generates a new administrative Bearer token.                            |
| `--quiet`          | `-q`      | Suppresses verbose log formatting and prints the raw token string only. |
| `--revoke-token`   |           | Revokes an existing Bearer token by string value.                       |
| `--prune`          |           | Interactively deletes unused roles and collection archives.             |
| `--version`        |           | Prints the version (e.g. `v0.5.0` or `dev`).                            |

---

## Configuration (`/etc/orbitron/config.yml`)

```yaml
listen_addr: "127.0.0.1:8080"
storage_path: "/var/lib/orbitron/storage"
log_path: "/var/log/orbitron/orbitron.log"
tokens_file: "/etc/orbitron/tokens.json"

# Set to true to require Bearer Token / Basic Auth for client pulls
require_auth_pull: false

# Maximum number of concurrent download/clone workers spawned during syncs
max_concurrency: 4

# Default lifetime of newly generated administrative tokens in days.
# 0 disables expiry so tokens never expire. Per-token TTLs can be
# overridden with the HTTP token API.
token_ttl_days: 0
```

---

## Mirroring Content (Server Ingestion)

Content is mirrored by posting YAML requirement manifests to Orbitron via cURL.
Orbitron stores the manifests and immediately triggers a background parallel download worker.
Each manifest is persisted under a content hash, so distinct manifests coexist on disk while
identical re-submissions are deduplicated. All stored manifests are replayed on full re-sync
(`/api/v1/sync`) and considered by `--prune`.

### 1. Mirroring Roles

**Role Manifest (`roles_requirements.yml`):**

```yaml
roles:
  - name: geerlingguy.nginx
    version: 3.2.0
  - name: geerlingguy.docker
    version: 7.1.0
  - name: RHEL9-CIS
    src: https://github.com/ansible-lockdown/RHEL9-CIS.git
    version: 2.0.2
```

**Ingest Command:**

```bash
curl -X POST http://127.0.0.1:8080/api/v1/requirements/roles \
  -H "Authorization: Bearer $ORBITRON_TOKEN" \
  -H "Content-Type: text/yaml" \
  --data-binary @roles_requirements.yml
```

### 2. Mirroring Collections

**Collection Manifest (`collections_requirements.yml`):**

```yaml
collections:
  - name: community.general
    version: 8.5.0
  - name: containers.podman
    version: 1.12.0
```

**Ingest Command:**

```bash
curl -X POST http://127.0.0.1:8080/api/v1/requirements/collections \
  -H "Authorization: Bearer $ORBITRON_TOKEN" \
  -H "Content-Type: text/yaml" \
  --data-binary @collections_requirements.yml
```

### 3. Version Specifiers

The `version` field accepts the same range specifiers Ansible does. Orbitron
resolves them against the published Galaxy indexes at sync time and stores the
highest matching release (roles are mirrored as git tags, collections as tarballs).

| Specifier            | Meaning                                     | Example         |
| -------------------- | ------------------------------------------- | --------------- |
| *(empty)* / `latest` | Highest published version                   | `latest`        |
| `==1.4.5` / `1.4.5`  | Exact version (or tag/branch for git `src`) | `1.4.5`         |
| `>=1.0.0`            | At least 1.0.0                              | `>=1.0.0`       |
| `>1.0.0,<2.0.0`      | Ranges AND-combined                         | `>1.0.0,<2.0.0` |
| `~=1.4.5`            | Compatible release (>=1.4.5, prefix 1.4)    | `~=1.4.5`       |
| `!=2.0.0`            | Anything but 2.0.0                          | `!=2.0.0`       |
| `==1.4.*` / `1.*`    | Wildcard prefix match                       | `==1.4.*`       |

```yaml
roles:
  - name: geerlingguy.nginx
    version: ">=2.0.0"
collections:
  - name: community.general
    version: "~=8.0"
```

### 4. Force Full Re-Sync

To re-sync all persisted manifests stored on the Orbitron server:

```bash
curl -X POST http://127.0.0.1:8080/api/v1/sync \
  -H "Authorization: Bearer $ORBITRON_TOKEN"
```

### 5. Deleting Cached Versions

Orbitron exposes authorized `DELETE` endpoints to remove a **single** cached version of a role or
collection (one artifact/directory plus its pin in the stored manifest). Endpoints require a valid
admin Bearer token; without one the request is rejected with `401`.

**Delete a single cached role version:**

```bash
curl -X DELETE http://127.0.0.1:8080/api/v1/storage/roles/geerlingguy.nginx/1.2.3 \
  -H "Authorization: Bearer $ORBITRON_TOKEN"
```

**Delete a single cached collection version:**

```bash
curl -X DELETE http://127.0.0.1:8080/api/v1/storage/collections/community.general/8.5.0 \
  -H "Authorization: Bearer $ORBITRON_TOKEN"
```

**Ansible Task Snippet:**

```yaml
- name: Delete a cached role version from Orbitron
  ansible.builtin.uri:
    url: "http://127.0.0.1:8080/api/v1/storage/roles/geerlingguy.nginx/1.2.3"
    method: DELETE
    headers:
      Authorization: "Bearer {{ orbitron_token }}"
    status_code: 200

- name: Delete a cached collection version from Orbitron
  ansible.builtin.uri:
    url: "http://127.0.0.1:8080/api/v1/storage/collections/community.general/8.5.0"
    method: DELETE
    headers:
      Authorization: "Bearer {{ orbitron_token }}"
    status_code: 200
```

### Automated Ingestion via Ansible Playbook

You can also automate feeding Orbitron or triggering a re-sync directly within your Ansible playbooks using
the `ansible.builtin.uri` module.

**Playbook Example (`feed_orbitron.yml`):**

```yaml
---
- name: Feed and Sync Orbitron Mirror Daemon
  hosts: localhost
  connection: local
  vars:
    orbitron_url: "http://127.0.0.1:8080"
    orbitron_token: "{{ lookup('env', 'ORBITRON_TOKEN') }}"

  module_defaults:
    ansible.builtin.uri:
      method: POST
      headers:
        Authorization: "Bearer {{ orbitron_token }}"
        Content-Type: "text/yaml"
      status_code: 202

  tasks:
    - name: Push Role Requirements to Orbitron
      ansible.builtin.uri:
        url: "{{ orbitron_url }}/api/v1/requirements/roles"
        body: "{{ lookup('file', 'roles_requirements.yml') }}"

    - name: Push Collection Requirements to Orbitron
      ansible.builtin.uri:
        url: "{{ orbitron_url }}/api/v1/requirements/collections"
        body: "{{ lookup('file', 'collections_requirements.yml') }}"

    - name: Trigger Full Re-Sync on Orbitron
      ansible.builtin.uri:
        url: "{{ orbitron_url }}/api/v1/sync"
        headers:
          Authorization: "Bearer {{ orbitron_token }}"
      tags:
        - never
        - sync
```

**Run the Automation Playbook:**

```bash
export ORBITRON_TOKEN=$(sudo orbitron --generate-token -q)
ansible-playbook feed_orbitron.yml
```

---

## Ansible Collection (`chrisvanmeer.orbitron`)

The repository ships an official Ansible collection under
`ansible_collections/chrisvanmeer/orbitron` to install, configure, and operate
Orbitron without hand-written `curl`/`systemctl` steps. It covers the full
lifecycle: daemon installation and service management, token bootstrapping,
declarative mirroring, and day-two operations such as purging cached versions.

### What it provides

* **Six HTTP modules** – `orbitron_info` (facts: health, storage inventory,
  manifests, sync status, tokens), `orbitron_token` (create/rotate/revoke),
  `orbitron_manifest` (store role/collection requirements), `orbitron_sync`
  (trigger a full sync, optionally wait), `orbitron_purge` (remove one cached
  version), `orbitron_prune` (invoke the gated prune API). All modules accept
  `url`, `token` (or `ORBITRON_TOKEN`), `validate_certs`, and `timeout`.
* **`chrisvanmeer.orbitron.orbitron` role** – end-to-end daemon install:
  resolves and downloads the release binary, runs `orbitron --install` to
  bootstrap the system user/dirs/systemd/logrotate, renders
  `/etc/orbitron/config.yml`, ensures the service is healthy, and generates (or
  reuses) an initial admin token. Supports full uninstallation with
  `orbitron_state: absent`.
* **`chrisvanmeer.orbitron.orbitron_mirror` role** – declarative mirroring from
  structured role/collection lists or local requirements files, with an
  optional `orbitron_mirror_wait_sync` on completion.

### Installing the collection

```bash
# Build and install directly from this repository
ansible-galaxy collection build ansible_collections/chrisvanmeer/orbitron
ansible-galaxy collection install chrisvanmeer-orbitron-1.0.0.tar.gz
```

The full variable reference, module docs, and security notes live in the
collection's own `README.md`; example playbooks ship under
`ansible_collections/chrisvanmeer/orbitron/examples/`.

### Example playbook (`orbitron_daemon.yml`)

Install the daemon on a fresh host and mirror a set of roles/collections in one
run. The `orbitron` role generates the admin token on first install and exposes
it as `orbitron_admin_token`; the `orbitron_mirror` role consumes it through
`orbitron_mirror_token` (a vault value if you pre-set `orbitron_token`):

```yaml
---
- name: Install Orbitron and mirror content
  hosts: mirrors
  become: true
  vars:
    # Optional pre-set admin token (or the role generates one and stores it in
    # orbitron_token_path, default /root/.orbitron_token, mode 0600).
    orbitron_token: "{{ vault_orbitron_token }}"

  roles:
    - role: chrisvanmeer.orbitron.orbitron
      vars:
        orbitron_version: v0.5.0
        orbitron_listen_addr: 127.0.0.1:8080
        orbitron_token_ttl_days: 365
    - role: chrisvanmeer.orbitron.orbitron_mirror
      vars:
        orbitron_mirror_token: "{{ orbitron_admin_token | default(vault_orbitron_token) }}"
        orbitron_mirror_collections:
          - name: community.general
            version: "8.5.0"
          - name: containers.podman
            version: "1.12.0"
        orbitron_mirror_roles:
          - name: geerlingguy.nginx
            version: "3.4.3"
```

```bash
ansible-playbook -i hosts orbitron_daemon.yml
```

### Operating an existing daemon with the modules

Against a daemon that is already running, point tasks straight at the API using
the HTTP modules:

```yaml
- name: Facts about the running daemon
  chrisvanmeer.orbitron.orbitron_info:
    url: http://127.0.0.1:8080
    token: "{{ orbitron_token }}"

- name: Store a collection requirements manifest
  chrisvanmeer.orbitron.orbitron_manifest:
    type: collections
    content: |
      collections:
        - name: community.docker
          version: "3.6.0"
    token: "{{ orbitron_token }}"

- name: Trigger a full sync and wait for completion
  chrisvanmeer.orbitron.orbitron_sync:
    url: http://127.0.0.1:8080
    token: "{{ orbitron_token }}"
    wait: true
```

---

## Ansible Client Configuration

To configure `ansible-galaxy` to use Orbitron as its Galaxy server for both roles and collections,
place an `ansible.cfg` in your project root:

### `ansible.cfg`

```ini
[defaults]
roles_path = ./roles
collections_path = ./collections

[galaxy]
server_list = orbitron

[galaxy_server.orbitron]
url = http://127.0.0.1:8080/api/
# token = YOUR_ORBITRON_TOKEN  # Uncomment if require_auth_pull is set to true
```

### Fetching Roles & Collections with `ansible-galaxy`

**Install Roles:**

```bash
ansible-galaxy role install -r roles_requirements.yml
```

**Install Collections:**

```bash
ansible-galaxy collection install -r collections_requirements.yml
```

---

## Management & Operations API

Orbitron exposes several administrative endpoints for health checks, sync visibility, and token lifecycle
management. All management endpoints except `/healthz` require a valid Bearer/Basic authorization header.

### 1. Health Check (`/healthz`)

Unauthenticated liveness probe for orchestrators, load balancers, and uptime monitors. Returns `200`
`{"status":"ok"}` while the storage path is writable, and `503 {"status":"degraded"}` otherwise.

```bash
curl http://127.0.0.1:8080/healthz
```

### 2. Sync Status (`GET /api/v1/sync/status`)

Returns the currently running background sync (kind, progress, per-item failures) together with a bounded
history of recently finished syncs, so automation can wait on completion instead of polling metrics.

```bash
curl -H "Authorization: Bearer $ORBITRON_TOKEN" http://127.0.0.1:8080/api/v1/sync/status
```

```json
{
  "current": {"kind": "full", "status": "running", "total": 4, "done": 2, "failures": []},
  "history": []
}
```

### 3. Token Lifecycle (`/api/v1/tokens`)

| Method   | Path                          | Description                                               |
| :------- | :---------------------------- | :-------------------------------------------------------- |
| `POST`   | /api/v1/tokens                | Generate a token (optional `ttl_days` / `label` in body). |
| `GET`    | /api/v1/tokens                | List token metadata (`?full=true` reveals the secrets).   |
| `DELETE` | /api/v1/tokens/{token}        | Revoke a token.                                           |
| `POST`   | /api/v1/tokens/{token}/rotate | Replace a token, revoking the original atomically.        |

**Create a token with a 30-day lifetime:**

```bash
curl -X POST http://127.0.0.1:8080/api/v1/tokens \
  -H "Authorization: Bearer $ORBITRON_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"ttl_days": 30, "label": "ci"}'
```

Tokens honour the global `token_ttl_days` configuration by default; an explicit `ttl_days: 0` in the request
body disables expiry for that token. Expired tokens are rejected by every authenticated endpoint and pruned
from the token store at daemon startup.

### 4. Stored Requirements Manifests (`GET /api/v1/manifests`)

Lists every requirements manifest currently stored on the mirror, grouped by type, with the content hash
(`sha256`) used for idempotent ingestion:

```bash
curl -H "Authorization: Bearer $ORBITRON_TOKEN" http://127.0.0.1:8080/api/v1/manifests
```

```json
{
  "roles": [{"file": "roles_x_requirements.yml", "sha256": "737690a6..."}],
  "collections": []
}
```

### 5. Storage Inventory (`GET /api/v1/storage`)

Returns the cached-version inventory of every role (directory) and collection (archive) on disk, including
which versions are actively declared by a stored manifest and their physical size in bytes:

```bash
curl -H "Authorization: Bearer $ORBITRON_TOKEN" http://127.0.0.1:8080/api/v1/storage
```

```json
{
  "roles": [
    {"type": "role", "name": "geerlingguy.nginx", "versions": [{"version": "1.2.3", "declared": true, "size_bytes": 20480}]}
  ],
  "collections": []
}
```

### 6. Prune API (`POST /api/v1/prune`)

Reserved for a future storage pruning endpoint. Until it is enabled, the route answers with
`501 {"status":"for_future_use"}` and performs no action.

---

## Reverse Proxy & Security (Recommended)

While Orbitron supports Bearer tokens and HTTP Basic Auth, it natively serves traffic over standard HTTP.
If you are mirroring private Git repositories using Personal Access Tokens (PATs) embedded in the `src` URL
(e.g., `https://oauth2:<TOKEN>@github.com/...`), these tokens could be exposed in transit.

It is highly recommended to run Orbitron behind a reverse proxy like **Nginx** configured with TLS/SSL.
This ensures all API calls, token transmissions, Web UI logins, and Git credentials remain securely encrypted.

### Nginx Example Configuration

Below is a standard Nginx reverse proxy configuration that secures
Orbitron with HTTPS and redirects all HTTP traffic:

```nginx
# Redirect HTTP to HTTPS
server {
    listen 80;
    server_name orbitron.example.com;
    return 301 https://$host$request_uri;
}

# HTTPS Server
server {
    listen 443 ssl;
    server_name orbitron.example.com;

    # SSL Certificates (e.g., Let's Encrypt)
    ssl_certificate /etc/letsencrypt/live/https://orbitron.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/https://orbitron.example.com/privkey.pem;

    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_ciphers HIGH:!aNULL:!MD5;

    location / {
        proxy_pass http://127.0.0.1:8080;

        # Forward essential headers to Orbitron
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;

        # Disable buffering for smooth on-the-fly .tar.gz streaming
        proxy_buffering off;

        # Allow large payloads if pushing massive requirement manifests
        client_max_body_size 10M;
    }
}
```

*Note: If you use a reverse proxy, ensure your `ansible.cfg` points to the `https://` address.*

---

## Prometheus Telemetry & Metrics

Orbitron exposes daemon and storage metrics at `/metrics` using standard Prometheus exposition format
(`text/plain; version=0.0.4`). Access to this endpoint requires Bearer Token or HTTP Basic Auth.

### Exposed Metrics

| Metric                         | Type    | Description                                         |
| :----------------------------- | :------ | :-------------------------------------------------- |
| `orbitron_uptime_seconds`      | Counter | Total daemon uptime in seconds.                     |
| `orbitron_roles_total`         | Gauge   | Total number of stored Ansible role directories.    |
| `orbitron_collections_total`   | Gauge   | Total number of stored Ansible collection archives. |
| `orbitron_manifests_total`     | Gauge   | Total number of stored requirement manifests.       |
| `orbitron_active_tokens_total` | Gauge   | Total number of registered Bearer tokens.           |

### Scraping Metrics

**cURL via Bearer Token:**

```bash
curl -H "Authorization: Bearer $ORBITRON_TOKEN" http://127.0.0.1:8080/metrics
```

**cURL via Basic Auth:**

```bash
curl -u "token:$ORBITRON_TOKEN" http://127.0.0.1:8080/metrics
```

**Prometheus Configuration Example (`prometheus.yml`):**

```yaml
scrape_configs:
  - job_name: 'orbitron'
    metrics_path: '/metrics'
    authorization:
      credentials: 'YOUR_ORBITRON_TOKEN'
    static_configs:
      - targets: ['127.0.0.1:8080']
```

---

## Author

Chris van Meer - <chris@atcomputing.nl>

---

## License

This project is licensed under the GNU General Public License v3.0 (see the
top-level `LICENSE` file). The Ansible collection in `ansible_collections/` is
licensed under the same terms, as noted in `COPYING`.
