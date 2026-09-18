#!/usr/bin/python
# -*- coding: utf-8 -*-
# Copyright (c) Ansible project contributors
# GNU General Public License v3.0+ (see LICENSES/GPL-3.0-or-later.txt or https://www.gnu.org/licenses/gpl-3.0.txt)
# SPDX-License-Identifier: GPL-3.0-or-later

DOCUMENTATION = r"""
---
module: orbitron_prune
short_description: Invoke the Orbitron HTTP prune endpoint
version_added: "1.0.0"
description:
  - Calls the Orbitron HTTP prune API.
  - The endpoint is currently gated on the daemon and answers with a 501
    for_future_use placeholder without side effects. This module surfaces that
    state cleanly (changed=false, state=for_future_use) instead of failing,
    so playbooks remain deployable against current and future daemons alike.
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
      - Request a dry run (lists candidates without deleting) once the daemon
        enables the endpoint. Ignored while the endpoint is gated.
    type: bool
    default: true
  validate_certs:
    description: Validate TLS certificates when C(url) is a https endpoint.
    type: bool
    default: false
  timeout:
    description: Timeout in seconds for each API call.
    type: int
    default: 10
notes:
  - Because the endpoint is gated on the daemon, this module never deletes
    anything until Orbitron enables it.
"""

EXAMPLES = r"""
- name: Discover the prune API contract
  chrisvanmeer.orbitron.orbitron_prune:
    token: "{{ orbitron_admin_token }}"
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
        - for_future_use while the daemon gates the endpoint, otherwise
          completed or dry_run.
      type: str
      sample: for_future_use
    candidates:
      description: Prune candidates (when the endpoint is enabled).
      type: list
      elements: str
    freed_bytes:
      description: Storage freed by the prune, zero for dry runs.
      type: int
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
    )
    argument_spec.update(COMMON_ARG_SPEC)

    module = AnsibleModule(argument_spec=argument_spec, supports_check_mode=True)
    client = OrbitronClient(module)

    payload = {"dry_run": module.params["dry_run"]}

    try:
        if module.check_mode:
            module.exit_json(changed=False, orbitron={"state": "check_mode", "candidates": [], "freed_bytes": 0})

        status, body = client.post("/api/v1/prune", payload=payload)
        if status == 501 or body.get("status") == "for_future_use":
            module.exit_json(
                changed=False,
                orbitron={
                    "state": "for_future_use",
                    "candidates": [],
                    "freed_bytes": 0,
                    "message": body.get("message", "prune API is not yet available"),
                },
            )

        module.exit_json(
            changed=True,
            orbitron={
                "state": "completed" if not module.params["dry_run"] else "dry_run",
                "candidates": body.get("candidates", body.get("items", [])),
                "freed_bytes": body.get("freed_bytes", 0),
            },
        )
    except OrbitronError as exc:
        if exc.status == 501:
            module.exit_json(
                changed=False,
                orbitron={"state": "for_future_use", "candidates": [], "freed_bytes": 0, "message": exc.body},
            )
        module.fail_json(msg="prune operation failed: %s" % exc.message, status=exc.status, body=exc.body)


if __name__ == "__main__":
    main()
