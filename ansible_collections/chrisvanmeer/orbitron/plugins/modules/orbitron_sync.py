#!/usr/bin/python
# -*- coding: utf-8 -*-
# Copyright (c) Ansible project contributors
# GNU General Public License v3.0+ (see LICENSES/GPL-3.0-or-later.txt or https://www.gnu.org/licenses/gpl-3.0.txt)
# SPDX-License-Identifier: GPL-3.0-or-later

DOCUMENTATION = r"""
---
module: orbitron_sync
short_description: Trigger and optionally wait for an Orbitron sync
version_added: "1.0.0"
description:
  - Triggers a full re-sync of every stored requirements manifest on an
    Orbitron mirror and, when C(wait=true), polls the sync status until the
    job finishes.
  - With C(skip_if_running=true) (the default) the module is idempotent while
    a sync is already running, reporting changed=false and starting no new job.
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
  wait:
    description: Wait for the triggered sync job to finish.
    type: bool
    default: true
  timeout:
    description:
      - Maximum number of seconds to wait for the sync to finish when
        C(wait=true).
      - Note that this overrides the API call timeout only for polling; the
        API timeout is still governed by the C(api_timeout) of the shared
        connection arguments.
    type: int
    default: 600
  skip_if_running:
    description: Do not start a new sync while one is already running.
    type: bool
    default: true
  validate_certs:
    description: Validate TLS certificates when C(url) is a https endpoint.
    type: bool
    default: false
notes:
  - A full sync always re-fetches declared content from the upstream galaxy.
    Keep C(skip_if_running=true) in recurring playbooks to avoid busying the
    daemon.
"""

EXAMPLES = r"""
- name: Trigger a full sync and wait for completion
  chrisvanmeer.orbitron.orbitron_sync:
    token: "{{ orbitron_admin_token }}"
    wait: true
"""

RETURN = r"""
sync_status:
  description: The final sync snapshot observed by the module.
  returned: success
  type: dict
  contains:
    current:
      description: The running job, or null once the sync finished.
      type: dict
    history:
      description: Recently finished sync jobs, newest first.
      type: list
      elements: dict
state:
  description: running, triggered, or idle.
  returned: success
  type: str
  sample: triggered
"""

import time

from ansible.module_utils.basic import AnsibleModule

from ansible_collections.chrisvanmeer.orbitron.plugins.module_utils.orbitron import (
    COMMON_ARG_SPEC,
    OrbitronClient,
    OrbitronError,
    fail_orbitron,
)


def main():
    argument_spec = dict(COMMON_ARG_SPEC)
    argument_spec.update(
        wait=dict(type="bool", default=True),
        timeout=dict(type="int", default=600),
        skip_if_running=dict(type="bool", default=True),
    )
    module = AnsibleModule(argument_spec=argument_spec, supports_check_mode=True)
    client = OrbitronClient(module)

    wait = module.params["wait"]
    deadline = module.params["timeout"]
    skip_if_running = module.params["skip_if_running"]

    try:
        snapshot = client.get("/api/v1/sync/status")[1]
        current = snapshot.get("current") or {}
        running = current.get("status") == "running"

        if running and skip_if_running:
            module.exit_json(changed=False, state="running", sync_status=snapshot)

        if module.check_mode:
            module.exit_json(changed=True, state="would_trigger", sync_status=snapshot)

        body = client.post("/api/v1/sync")[1]
        state = body.get("status", "triggered")

        final_snapshot = snapshot
        if wait:
            deadline_start = time.time()
            while True:
                final_snapshot = client.get("/api/v1/sync/status")[1]
                current_job = final_snapshot.get("current")
                if not current_job or current_job.get("status") != "running":
                    break
                if time.time() - deadline_start > deadline:
                    module.fail_json(
                        msg="sync did not complete within %d seconds" % deadline,
                        state="running",
                        sync_status=final_snapshot,
                    )
                time.sleep(1)

        module.exit_json(changed=True, state=state, sync_status=final_snapshot)
    except OrbitronError as exc:
        fail_orbitron(module, exc, "sync operation")


if __name__ == "__main__":
    main()
