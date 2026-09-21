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
ansible-galaxy collection install chrisvanmeer.orbitron:1.3.0
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
| `orbitron_prune` | Prune unserved cached versions via the access-based prune API (`days` retention window, dry-run by default) |

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
        # Leave orbitron_version unset to install the latest release.
        orbitron_version: v1.3.2
        orbitron_listen_addr: 0.0.0.0:8080
        orbitron_token_ttl_days: 90
        # Optionally prune versions not served within 60 days (dry-run preview
        # by default; set orbitron_prune_dry_run: false to delete for real).
        orbitron_prune_days: 60
```

After the run:

- `orbitron_admin_token` holds the effective admin token (either your
  `orbitron_token` vault value or the one generated and stored in
  `orbitron_token_path`, default `/root/.orbitron_token`, mode 0600).
- `orbitron_version` holds the installed version.

Key variables: `orbitron_version` (`latest` or exact tag), `orbitron_binary_src`
(air-gapped installs), `orbitron_checksum`, the `orbitron_*` daemon config
keys, the forward-proxy keys `orbitron_http_proxy`, `orbitron_https_proxy`,
and `orbitron_no_proxy` (see the main README's "Forward Proxy" section),
`orbitron_state` (`present`/`absent`), and the token bootstrap variables.
Access-based storage pruning is optional via `orbitron_prune_days` (retention
window; `0` disables it) and `orbitron_prune_dry_run` (defaults to `true`, so
only the candidates are previewed).

#### SSO / OIDC (Keycloak)

Optional SSO login for the web dashboard is configured through
`orbitron_oidc_enabled` plus the `orbitron_oidc_issuer` (full Keycloak realm
URL, e.g. `https://keycloak.example.org/realms/orbitron`),
`orbitron_oidc_client_id` and `orbitron_oidc_client_secret`. `orbitron_oidc_session_ttl_hours`
(default `8`) sets the dashboard session lifetime and `orbitron_oidc_redirect_uri`
an explicit callback URL (default: auto-derived from the request as
`<scheme>://<host>/ui/oidc/callback`). To admit only members of specific
Keycloak groups, set `orbitron_oidc_allowed_groups` to a list such as
`["orbitron-admins"]`; empty (default) allows every verified SSO user in.
Deploy with these variables set, or `set_fact` them after a later run and
re-run the role — the daemon picks the changes up (optionally hot via
`systemctl reload orbitron`). Full group paths emitted by Keycloak's Group
Membership mapper (e.g. `/admins`) match plain `allowed_groups` names as well.
See the main README's
["SSO / OIDC (Keycloak)"](../../../README.md#sso--oidc-keycloak) section for
the Keycloak client setup guide (redirect URIs, client secret, and the
`groups` claim required for the group filter).

#### Forward proxy

When Orbitron must reach Galaxy, GitHub and git remotes through a corporate
proxy (e.g. Squid), set `orbitron_http_proxy` and `orbitron_https_proxy` to the
proxy URL such as `http://squid.internal:3128`, and `orbitron_no_proxy` to a
comma-separated list of hosts/domains/CIDRs that must bypass the proxy (e.g.
`localhost,127.0.0.1,.internal`). These map directly onto the daemon's
`http_proxy`, `https_proxy` and `no_proxy` config keys, which fall back to the
process `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` environment when left empty. See
the main README's ["Forward Proxy"](../../../README.md#forward-proxy-squid-co)
section for details. Example:

```yaml
orbitron_http_proxy: http://squid.internal:3128
orbitron_https_proxy: http://squid.internal:3128
orbitron_no_proxy: "localhost,127.0.0.1,.internal,10.0.0.0/8"
```

### `orbitron_mirror`

Declarative mirroring: stores role/collection requirements and waits for the
sync. The role is idempotent — re-running an unchanged, fully mirrored
playbook reports no changes, because the sync only runs when a declared
requirement is not mirrored at its exact version yet (set
`orbitron_mirror_sync_force: true` to force a full re-sync regardless).
Pinned versions that are already mirrored report `ok`; empty, `latest`, and
expression-style versions always warrant a re-check.

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
            version: "3.3.1"
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
