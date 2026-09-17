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
* **On-the-Fly Archiving**: Packages unpacked Git-based roles into `.tar.gz` streams on-the-fly during download.
* **Parallel Background Sync**: Concurrent fetching of roles, collections, and shallow Git clones (`--depth 1`).
* **Flexible Authentication**: Unauthenticated pulls by default with an optional `require_auth_pull: true` toggle
  supporting Bearer tokens and HTTP Basic Auth.
* **Automated Lifecycle Management**: Integrated CLI commands for user creation (`orbitron:orbitron`),
  systemd service registration, logrotate configuration, and full uninstallation cleanup.
* **Storage Pruner**: Built-in CLI flag (`--prune`) to scan manifests, detect orphaned versions, and clean up
  disk usage.

---

## Installation & Service Management

### Building from Source

```bash
make build
```

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
```

| Flag               | Shorthand | Description                                                             |
| :----------------- | :-------- | :---------------------------------------------------------------------- |
| `--generate-token` |           | Generates a new administrative Bearer token.                            |
| `--quiet`          | `-q`      | Suppresses verbose log formatting and prints the raw token string only. |
| `--revoke-token`   |           | Revokes an existing Bearer token by string value.                       |
| `--prune`          |           | Interactively deletes unused roles and collection archives.             |

---

## Configuration (`/etc/orbitron/config.yml`)

```yaml
listen_addr: "127.0.0.1:8080"
storage_path: "/var/lib/orbitron/storage"
log_path: "/var/log/orbitron/orbitron.log"
tokens_file: "/etc/orbitron/tokens.json"

# Set to true to require Bearer Token / Basic Auth for client pulls
require_auth_pull: false
```

---

## Mirroring Content (Server Ingestion)

Content is mirrored by posting YAML requirement manifests to Orbitron via cURL.
Orbitron stores the manifests and immediately triggers a background parallel download worker.

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

### 3. Force Full Re-Sync

To re-sync all persisted manifests stored on the Orbitron server:

```bash
curl -X POST http://127.0.0.1:8080/api/v1/sync \
  -H "Authorization: Bearer $ORBITRON_TOKEN"
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

## Reverse Proxy & Security (Recommended)

While Orbitron supports Bearer tokens and HTTP Basic Auth, it natively serves traffic over standard HTTP.
If you are mirroring private Git repositories using Personal Access Tokens (PATs) embedded in the `src` URL
(e.g., `https://oauth2:<TOKEN>@github.com/...`), these tokens could be exposed in transit.

It is highly recommended to run Orbitron behind a reverse proxy like **Nginx** configured with TLS/SSL.
This ensures all API calls, token transmissions, and Git credentials remain securely encrypted.

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
    ssl_certificate /etc/letsencrypt/live/orbitron.example.com/fullchain.pem;
    ssl_certificate_key /etc/letsencrypt/live/orbitron.example.com/privkey.pem;

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

This project is licensed under the MIT License.
