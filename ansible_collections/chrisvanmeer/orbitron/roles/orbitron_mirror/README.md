# orbitron_mirror role

Declaratively mirrors Ansible roles and collections to a running
[Orbitron](https://github.com/chrisvanmeer/orbitron) daemon.

## Requirements

- A reachable, healthy Orbitron daemon
- A valid admin token (see the `orbitron` role or the token module)
- Ansible Core >= 2.16 on the controller

## Role variables

| Variable                                   | Default                 | Description                                                           |
| :----------------------------------------- | :---------------------- | :-------------------------------------------------------------------- |
| `orbitron_mirror_url`                      | `http://127.0.0.1:8080` | Daemon management API URL.                                            |
| `orbitron_mirror_token`                    | `""`                    | Admin token; required.                                                |
| `orbitron_mirror_roles`                    | `[]`                    | Structured list of role requirements.                                 |
| `orbitron_mirror_collections`              | `[]`                    | Structured list of collection requirements.                           |
| `orbitron_mirror_role_manifest_path`       | `""`                    | Path to a role requirements file.                                     |
| `orbitron_mirror_collection_manifest_path` | `""`                    | Path to a collection requirements file.                               |
| `orbitron_mirror_wait_sync`                | `true`                  | Wait for the triggered sync to finish.                                |
| `orbitron_mirror_sync_force`               | `false`                 | Always trigger a full sync, even when no manifest changed.            |

Provide either the structured `orbitron_mirror_roles` /
`orbitron_mirror_collections` lists or a requirements file path, and set
`orbitron_mirror_token`.

The role is idempotent: a sync is only triggered when a stored manifest
changed in this run (or when `orbitron_mirror_sync_force` is set), so repeat
runs of an unchanged playbook report no changes.

## Example

```yaml
- hosts: mirrors
  roles:
    - role: chrisvanmeer.orbitron.orbitron_mirror
      vars:
        orbitron_mirror_token: "{{ vault_orbitron_token }}"
        orbitron_mirror_collections:
          - name: community.general
            version: "8.5.0"
        orbitron_mirror_roles:
          - name: geerlingguy.nginx
            version: "3.4.3"
```

## License

GPL-3.0-or-later
