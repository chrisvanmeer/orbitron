#!/usr/bin/python
# -*- coding: utf-8 -*-
# Copyright (c) Ansible project contributors
# GNU General Public License v3.0+ (see LICENSES/GPL-3.0-or-later.txt or https://www.gnu.org/licenses/gpl-3.0.txt)
# SPDX-License-Identifier: GPL-3.0-or-later

DOCUMENTATION = r"""
---
module: orbitron_info
short_description: Gather facts from an Orbitron mirror daemon
version_added: "1.0.0"
description:
  - Gathers read-only facts about a running Orbitron mirror daemon, covering
    service health, the cached storage inventory, stored requirements
    manifests, current/recent sync jobs, and registered admin tokens (the
    secrets themselves are never returned).
  - Health facts are available without a token; all other facts require a
    valid administrative token.
author:
  - Chris van Meer (@chrisvanmeer)
options:
  url:
    description: Base URL of the Orbitron management API.
    type: str
    default: "http://127.0.0.1:8080"
  token:
    description:
      - Administrative token.
      - Required to read anything beyond health facts. May also be provided
        through the ORBITRON_TOKEN environment variable.
    type: str
    required: false
  validate_certs:
    description: Validate TLS certificates when C(url) is a https endpoint.
    type: bool
    default: false
  timeout:
    description: Timeout in seconds for each API call.
    type: int
    default: 10
requirements:
  - ansible >= 2.16
notes:
  - Token values are never returned by this module.
"""

EXAMPLES = r"""
- name: Gather Orbitron facts
  chrisvanmeer.orbitron.orbitron_info:
    url: https://mirror.example.com
    token: "{{ orbitron_admin_token }}"
  register: orbitron

- name: Fail when the mirror is unhealthy
  ansible.builtin.assert:
    that:
      - orbitron.orbitron.health.status == "ok"
"""

RETURN = r"""
orbitron:
  description: Facts gathered from the Orbitron daemon.
  returned: always
  type: dict
  contains:
    health:
      description: Liveness and storage-writability probe results.
      returned: always
      type: dict
      contains:
        status:
          description: ok or degraded.
          type: str
          sample: ok
        storage_writable:
          description: Whether the daemon can still write to its storage path.
          type: bool
          sample: true
        http_status:
          description: Raw HTTP status of the health probe.
          type: int
          sample: 200
    sync_status:
      description: Current and recent background sync jobs.
      returned: when a token is supplied
      type: dict
    manifests:
      description: Stored requirements manifests, keyed by type.
      returned: when a token is supplied
      type: dict
    storage:
      description: Cached roles and collections inventory.
      returned: when a token is supplied
      type: dict
    tokens:
      description: Registered admin tokens without secrets.
      returned: when a token is supplied
      type: list
      elements: dict
      sample: [{"created_at": 1720000000, "expires_at": 0, "label": "ci"}]
"""

from ansible.module_utils.basic import AnsibleModule

from ansible_collections.chrisvanmeer.orbitron.plugins.module_utils.orbitron import (
    COMMON_ARG_SPEC,
    OrbitronClient,
    OrbitronError,
)


def _safe_get(client, path):
    """Fetch facts gracefully so one failing endpoint does not hide the rest."""
    try:
        status_code, body = client.get(path)
        body["http_status"] = status_code
        return body
    except OrbitronError as exc:
        return {"error": str(exc), "http_status": exc.status}


def main():
    module = AnsibleModule(
        argument_spec=COMMON_ARG_SPEC,
        supports_check_mode=True,
    )
    client = OrbitronClient(module, token_required=False)

    facts = {"health": _safe_get(client, "/healthz")}

    if client.token:
        facts["sync_status"] = _safe_get(client, "/api/v1/sync/status")
        facts["manifests"] = _safe_get(client, "/api/v1/manifests")
        facts["storage"] = _safe_get(client, "/api/v1/storage")
        token_body = _safe_get(client, "/api/v1/tokens")
        facts["tokens"] = token_body.get("tokens", [])

    module.exit_json(changed=False, orbitron=facts)


if __name__ == "__main__":
    main()
