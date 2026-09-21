# orbitron role

Installs and manages the [Orbitron](https://github.com/chrisvanmeer/orbitron)
Galaxy mirror daemon on a Linux host with `systemd`.

## Requirements

- Linux on `x86_64` or `aarch64` with `systemd`
- Root access (`become: true`)
- Outbound HTTPS to GitHub releases (unless `orbitron_binary_src` is used)

## Role variables

| Variable                     | Default                          | Description                                              |
| :--------------------------- | :------------------------------- | :------------------------------------------------------- |
| `orbitron_state`             | `present`                        | `present` installs, `absent` uninstalls.                 |
| `orbitron_version`           | `latest`                         | Exact release tag to install, e.g. `v1.2.0`.             |
| `orbitron_binary_src`        | `""`                             | Local binary path for air-gapped installs.               |
| `orbitron_checksum`          | `""`                             | SHA-256 checksum for downloaded binaries.                |
| `orbitron_stage_dir`         | `/var/tmp/orbitron-ansible`      | Temporary staging dir for the binary before `--install`. |
| `orbitron_install_git`       | `true`                           | Install `git` for git-backed requirements.               |
| `orbitron_listen_addr`       | `127.0.0.1:8080`                 | Daemon listen address.                                   |
| `orbitron_storage_path`      | `/var/lib/orbitron/storage`      | Mirror cache directory.                                  |
| `orbitron_log_path`          | `/var/log/orbitron/orbitron.log` | Daemon log file.                                         |
| `orbitron_tokens_file`       | `/etc/orbitron/tokens.json`      | Token store path.                                        |
| `orbitron_require_auth_pull` | `false`                          | Require a token for client pulls.                        |
| `orbitron_max_concurrency`   | `4`                              | Parallel sync workers.                                   |
| `orbitron_token_ttl_days`    | `0`                              | Default token lifetime in days (`0` = never).            |
| `orbitron_http_proxy`        | `""`                             | Forward proxy for outbound `http://` (e.g. Squid).       |
| `orbitron_https_proxy`       | `""`                             | Forward proxy for outbound `https://` traffic.           |
| `orbitron_no_proxy`          | `""`                             | Comma-separated proxy exclusions (host, domain, CIDR).   |
| `orbitron_oidc_enabled`      | `false`                          | Enable SSO (OIDC) login on the web dashboard.            |
| `orbitron_oidc_issuer`       | `""`                             | Full Keycloak realm URL, e.g. `https://kc/realms/x`.     |
| `orbitron_oidc_client_id`    | `""`                             | Keycloak confidential client id.                         |
| `orbitron_oidc_client_secret`| `""`                             | Keycloak client secret.                                  |
| `orbitron_oidc_session_ttl_hours` | `8`                          | Dashboard session lifetime in hours.                     |
| `orbitron_oidc_redirect_uri` | `""`                             | Explicit redirect URI, else auto-derived.                |
| `orbitron_oidc_allowed_groups` | `[]`                          | Optional group filter; members of these groups only.     |
| `orbitron_token`             | `""`                             | Pre-set admin token (else generated).                    |
| `orbitron_token_path`        | `/root/.orbitron_token`          | Where a generated token is persisted (0600).             |
| `orbitron_prune_days`        | `0`                              | Retention window in days; `> 0` enables pruning.         |
| `orbitron_prune_dry_run`     | `true`                           | Preview candidates only instead of deleting.             |

The role stages the binary in `orbitron_stage_dir` before running `--install`
and removes the staging directory again afterwards. Because the staged binary
is executed, the directory must be located on a mount that permits execution.
If your `/var` (or `/var/tmp`) filesystem is mounted with `noexec`, point
`orbitron_stage_dir` elsewhere, for example `/opt/orbitron/stage`.

Set `orbitron_prune_days` to a value above `0` to prune cached versions that
have not been served to a client within that retention window, right after the
daemon is installed. Pruning runs as a dry run by default (it only previews the
candidate versions); set `orbitron_prune_dry_run: false` to delete them for real.

## SSO (OIDC / Keycloak)

Set `orbitron_oidc_enabled: true` plus the `issuer`, `client_id` and
`client_secret` to let dashboard users authenticate through an external
identity provider instead of (or in addition to) a manual access token. The
issuer must be the full Keycloak realm URL
(`https://<keycloak>/realms/<realm>`), and the OIDC client must be registered
in Keycloak as **confidential** with redirect URI
`https://<your-orbitron-host>/ui/oidc/callback`. The regular token login stays
available; SSO sessions are validated identically and live in memory until the
daemon restarts. See the main [Orbitron README](../../../../../README.md#sso--oidc-keycloak)
for the Keycloak client setup guide.

To admit only members of specific Keycloak groups, set
`orbitron_oidc_allowed_groups` to a list such as
`["orbitron-admins", "orbitron-ops"]` — every other SSO user is denied access
(fails closed when the ID token carries no `groups` claim). Empty (the default)
disables the filter. The ID token only carries `groups` after a **Group
Membership** protocol mapper (or the built-in `groups` client scope) with
*Add to ID token* **ON** is added to the Keycloak client; full group paths
(e.g. `/admins`) emitted by default by the Group Membership mapper are matched
as well as plain names. See the main README's Keycloak-side setup for the
exact steps.

## Exposed facts

| Fact                   | Description                                    |
| :--------------------- | :--------------------------------------------- |
| `orbitron_admin_token` | Effective admin token (pre-set or generated).  |
| `orbitron_version`     | The installed Orbitron version.                |

## Example

```yaml
- hosts: mirrors
  become: true
  roles:
    - role: chrisvanmeer.orbitron.orbitron
      vars:
        orbitron_version: v1.2.0
        orbitron_listen_addr: 0.0.0.0:8080
        orbitron_token_ttl_days: 90
        orbitron_http_proxy: http://squid.internal:3128
        orbitron_https_proxy: http://squid.internal:3128
        orbitron_no_proxy: "localhost,127.0.0.1,.internal"
        # Prune versions not served within 60 days (dry-run preview by default).
        orbitron_prune_days: 60
        orbitron_prune_dry_run: true
```

## License

GPL-3.0-or-later
