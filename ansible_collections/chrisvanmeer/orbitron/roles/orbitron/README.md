# orbitron role

Installs and manages the [Orbitron](https://github.com/chrisvanmeer/orbitron)
Galaxy mirror daemon on a Linux host with `systemd`.

## Requirements

- Linux on `x86_64` or `aarch64` with `systemd`
- Root access (`become: true`)
- Outbound HTTPS to GitHub releases (unless `orbitron_binary_src` is used)

## Role variables

| Variable                     | Default                          | Description                                      |
| :--------------------------- | :------------------------------- | :----------------------------------------------- |
| `orbitron_state`             | `present`                        | `present` installs, `absent` uninstalls.         |
| `orbitron_version`           | `latest`                         | Exact release tag to install, e.g. `v1.2.0`.     |
| `orbitron_binary_src`        | `""`                             | Local binary path for air-gapped installs.       |
| `orbitron_checksum`          | `""`                             | SHA-256 checksum for downloaded binaries.        |
| `orbitron_install_git`       | `true`                           | Install `git` for git-backed requirements.       |
| `orbitron_listen_addr`       | `127.0.0.1:8080`                 | Daemon listen address.                           |
| `orbitron_storage_path`      | `/var/lib/orbitron/storage`      | Mirror cache directory.                          |
| `orbitron_log_path`          | `/var/log/orbitron/orbitron.log` | Daemon log file.                                 |
| `orbitron_tokens_file`       | `/etc/orbitron/tokens.json`      | Token store path.                                |
| `orbitron_require_auth_pull` | `false`                          | Require a token for client pulls.                |
| `orbitron_max_concurrency`   | `4`                              | Parallel sync workers.                           |
| `orbitron_token_ttl_days`    | `0`                              | Default token lifetime in days (`0` = never).    |
| `orbitron_token`             | `""`                             | Pre-set admin token (else generated).            |
| `orbitron_token_path`        | `/root/.orbitron_token`          | Where a generated token is persisted (0600).     |

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
```

## License

GPL-3.0-or-later
