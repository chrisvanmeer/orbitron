#!/usr/bin/python
# -*- coding: utf-8 -*-
# Copyright (c) Ansible project contributors
# GNU General Public License v3.0+ (see LICENSES/GPL-3.0-or-later.txt or https://www.gnu.org/licenses/gpl-3.0.txt)
# SPDX-License-Identifier: GPL-3.0-or-later

DOCUMENTATION = r"""
---
module: orbitron_prune
short_description: Prune stale cached versions from the Orbitron mirror
version_added: "1.0.0"
description:
  - Calls the Orbitron HTTP prune API and removes role/collection versions that
    have not been served to a client for at least C(days) days (access-based
    retention). Versions that were never requested are always kept.
  - A dry run (C(dry_run=true), the default) reports what would be removed
    without deleting anything, so playbooks can preview the candidate set
    before committing.
requirements:
  - ansible >= 2.16
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
      - May also be provided through the ORBITRON_TOKEN environment variable.
    type: str
    required: false
  dry_run:
    description:
      - When true (default), only report the versions that would be pruned.
      - When false, delete the stale versions for real.
    type: bool
    default: true
  days:
    description:
      - Retention window in days. A version is pruned when it has not been
        served within the last C(days) days. Defaults to 90.
    type: int
    default: 90
  validate_certs:
    description: Validate TLS certificates when C(url) is a https endpoint.
    type: bool
    default: false
  timeout:
    description: Timeout in seconds for each API call.
    type: int
    default: 10
"""

EXAMPLES = r"""
- name: Preview what the pruner would remove after 60 days
  chrisvanmeer.orbitron.orbitron_prune:
    token: "{{ orbitron_admin_token }}"
    days: 60
  register: prune

- name: Actually prune versions untouched for 90 days
  chrisvanmeer.orbitron.orbitron_prune:
    token: "{{ orbitron_admin_token }}"
    dry_run: false
  register: prune
"""

RETURN = r"""
orbitron:
  description: Result of the prune call.
  returned: success
  type: dict
  contains:
    state:
      description:
        - completed when C(dry_run=false), dry_run when C(dry_run=true).
      type: str
      sample: dry_run
    items:
      description: Absolute paths of the pruned (or prune-able) versions.
      type: list
      elements: str
      sample: ["/var/lib/orbitron/storage/roles/geerlingguy.nginx/2.0.1"]
    freed_bytes:
      description: Storage freed by the prune, zero for dry runs.
      type: int
      sample: 1048576
    executed:
      description: Whether the deletion actually ran (false for dry runs).
      type: bool
      sample: false
"""

from ansible.module_utils.basic import AnsibleModule

from ansible_collections.chrisvanmeer.orbitron.plugins.module_utils.orbitron import (
    COMMON_ARG_SPEC,
    OrbitronClient,
    OrbitronError,
)


def main():
    argument_spec = dict(
        dry_run=dict(type="bool", default=True),
        days=dict(type="int", default=90),
    )
    argument_spec.update(COMMON_ARG_SPEC)

    module = AnsibleModule(argument_spec=argument_spec, supports_check_mode=True)
    client = OrbitronClient(module)

    payload = {"dry_run": module.params["dry_run"], "days": module.params["days"]}

    try:
        if module.check_mode:
            module.exit_json(changed=False, orbitron={"state": "check_mode", "items": [], "freed_bytes": 0, "executed": False})

        status, body = client.post("/api/v1/prune", payload=payload)

        module.exit_json(
            changed=bool(body.get("executed")),
            orbitron={
                "state": "completed" if body.get("executed") else "dry_run",
                "items": body.get("items", body.get("candidates", [])),
                "freed_bytes": body.get("freed_bytes", 0),
                "executed": bool(body.get("executed")),
            },
        )
    except OrbitronError as exc:
        module.fail_json(msg="prune operation failed: %s" % exc.message, status=exc.status, body=exc.body)


if __name__ == "__main__":
    main()