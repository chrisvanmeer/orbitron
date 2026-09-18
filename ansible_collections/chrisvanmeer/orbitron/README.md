# chrisvanmeer.orbitron

Ansible collection to install, configure, and manage the
[Orbitron](https://github.com/chrisvanmeer/orbitron) Ansible Galaxy mirror
daemon.

## Installation

```bash
ansible-galaxy collection install chrisvanmeer.orbitron
```

Pin a specific version with:

```bash
ansible-galaxy collection install chrisvanmeer.orbitron:1.2.1
```

## Requirements

- Ansible Core >= 2.16 on the controller
- Linux targets (amd64 / arm64) with `systemd`
- A running Orbitron daemon exposing its management API for the
  `orbitron_*` modules (the `orbitron` role installs one for you)

## Modules

| Module | Purpose |
| --- | --- |
| `orbitron_info` | Read-only facts: health, storage inventory, manifests, sync status, tokens |
| `orbitron_token` | Create, rotate, and revoke admin tokens (idempotent by label) |
| `orbitron_manifest` | Store role/collection requirements manifests (idempotent by content hash) |
| `orbitron_sync` | Trigger a full sync and optionally wait for completion |
| `orbitron_purge` | Remove one cached role/collection version |
| `orbitron_prune` | Invoke the gated prune API and surface its `for_future_use` state cleanly |

All HTTP modules accept `url` (default `http://127.0.0.1:8080`), `token`
(also via `ORBITRON_TOKEN`), `validate_certs`, and `timeout`.

## Roles

### `orbitron`

Installs the daemon end to end: resolves and downloads the release binary,
bootstraps the system user/dirs/systemd through `orbitron --install`, renders
`/etc/orbitron/config.yml`, ensures the service runs and is healthy, and
generates (or reuses) an initial admin token.

```yaml
- hosts: mirrors
  become: true
  roles:
    - role: chrisvanmeer.orbitron.orbitron
      vars:
        orbitron_version: v0.5.0
        orbitron_listen_addr: 0.0.0.0:8080
        orbitron_token_ttl_days: 90
```

After the run:

- `orbitron_admin_token` holds the effective admin token (either your
  `orbitron_token` vault value or the one generated and stored in
  `orbitron_token_path`, default `/root/.orbitron_token`, mode 0600).
- `orbitron_version` holds the installed version.

Key variables: `orbitron_version` (`latest` or exact tag), `orbitron_binary_src`
(air-gapped installs), `orbitron_checksum`, the `orbitron_*` daemon config
keys, `orbitron_state` (`present`/`absent`), and the token bootstrap variables.

### `orbitron_mirror`

Declarative mirroring: stores role/collection requirements and waits for the
sync.

```yaml
- hosts: mirrors
  roles:
    - role: chrisvanmeer.orbitron.orbitron_mirror
      vars:
        orbitron_mirror_token: "{{ vault_orbitron_token }}"
        orbitron_mirror_collections:
          - name: community.general
            version: "8.4.0"
          - name: community.docker
            version: "3.6.0"
        orbitron_mirror_roles:
          - name: geerlingguy.nginx
            version: "3.4.3"
```

## Example playbooks

- `examples/install.yml` – full install, then mirror a couple of collections.
- `examples/manage_tokens.yml` – token lifecycle with an existing daemon.

## Security notes

- Store `orbitron_token` in a vault. Use `no_log: true` on tasks that capture
  freshly generated token secrets.
- Expose the management API `127.0.0.1`-only or behind a TLS reverse proxy;
  `validate_certs: true` is recommended for `https` URLs.
- The `orbitron` role runs `--install` and service commands, so it must run
  with `become: true` on the target.
