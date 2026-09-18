#!/usr/bin/python
# -*- coding: utf-8 -*-
# Copyright (c) Ansible project contributors
# GNU General Public License v3.0+ (see LICENSES/GPL-3.0-or-later.txt or https://www.gnu.org/licenses/gpl-3.0.txt)
# SPDX-License-Identifier: GPL-3.0-or-later

DOCUMENTATION = r"""
---
module: orbitron_token
short_description: Create, rotate and revoke Orbitron admin tokens
version_added: "1.0.0"
description:
  - Manages administrative tokens on an Orbitron mirror through its REST API.
  - With state=present the module is idempotent by resource label. When a
    requested label already exists no new token is created and no secret is
    emitted. With the extra keyword, the matching token is rotated and its new
    value returned. With state=absent the supplied token value is revoked.
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
      - Token used to authenticate against the API.
      - May also be provided through the ORBITRON_TOKEN environment variable.
    type: str
    required: false
  label:
    description:
      - Human-readable label under which the token is registered.
      - Used as the idempotency key for state=present.
    type: str
    required: false
  ttl_days:
    description:
      - Lifetime of a newly created or rotated token in days.
      - 0 or unset means the token never expires.
    type: int
    required: false
  state:
    description:
      - present ensures a token with the given label exists (creating it when
        missing).
      - absent revokes the token value supplied in O(token_value).
    type: str
    choices: [present, absent]
    default: present
  token_value:
    description:
      - The raw token value to rotate or revoke.
      - Required for state=absent, and for rotate=true.
    type: str
    required: false
  rotate:
    description:
      - When true and a token matching O(token_value) exists, rotate it and
        return the new value. A rotated token replaces the original and
        carries the same label and remaining lifetime.
    type: bool
    default: false
  validate_certs:
    description: Validate TLS certificates when C(url) is a https endpoint.
    type: bool
    default: false
  timeout:
    description: Timeout in seconds for each API call.
    type: int
    default: 10
notes:
  - The secret of a freshly created or rotated token is only visible in the
    result of the run that created it; Ansible never re-fetches it afterwards.
  - Set no_log=true on the task that captures a new token to avoid leaking it
    into logs.
"""

EXAMPLES = r"""
- name: Create (or reuse) an admin token labelled "ci"
  chrisvanmeer.orbitron.orbitron_token:
    token: "{{ bootstrap_token }}"
    label: ci
    ttl_days: 90
  register: ci_token

- name: Rotate the token for nominal dollars
  chrisvanmeer.orbitron.orbitron_token:
    token: "{{ ci_token.orbitron.token }}"
    token_value: "{{ ci_token.orbitron.token }}"
    rotate: true
  no_log: true

- name: Revoke a token
  chrisvanmeer.orbitron.orbitron_token:
    token: "{{ bootstrap_token }}"
    token_value: "{{ old_token }}"
    state: absent
"""

RETURN = r"""
orbitron:
  description: Result of the token operation.
  returned: success
  type: dict
  contains:
    token:
      description: Token value, only present for create/rotate operations.
      type: str
    label:
      description: Label of the affected token.
      type: str
    created_at:
      description: Unix timestamp of the token creation.
      type: int
    expires_at:
      description: Unix timestamp of expiry, 0 when it never expires.
      type: int
    exists:
      description: Whether a matching token already existed.
      type: bool
    state:
      description: Effective state of the operation.
      type: str
      sample: present
"""

from ansible.module_utils.basic import AnsibleModule

from ansible_collections.chrisvanmeer.orbitron.plugins.module_utils.orbitron import (
    COMMON_ARG_SPEC,
    OrbitronClient,
    OrbitronError,
    fail_orbitron,
)


def _list_tokens(client):
    body = client.get("/api/v1/tokens")[1]
    return body.get("tokens", [])


def _find_by_label(tokens, label):
    for entry in tokens:
        if entry.get("label") == label:
            return entry
    return None


def _find_by_value(tokens, value):
    for entry in tokens:
        if entry.get("token") == value:
            return entry
    return None


def main():
    argument_spec = dict(
        label=dict(type="str", required=False),
        ttl_days=dict(type="int", required=False),
        state=dict(type="str", default="present", choices=["present", "absent"]),
        token_value=dict(type="str", required=False, no_log=True),
        rotate=dict(type="bool", default=False),
    )
    argument_spec.update(COMMON_ARG_SPEC)

    module = AnsibleModule(argument_spec=argument_spec, supports_check_mode=True)
    client = OrbitronClient(module)

    label = module.params.get("label")
    ttl_days = module.params.get("ttl_days")
    state = module.params.get("state")
    token_value = module.params.get("token_value")
    rotate = module.params.get("rotate")

    payload = {}
    if label:
        payload["label"] = label
    if ttl_days is not None:
        payload["ttl_days"] = ttl_days

    try:
        if state == "absent":
            if not token_value:
                module.fail_json(msg="state=absent requires token_value")
            tokens = _list_tokens(client)
            if _find_by_value(tokens, token_value) is None:
                module.exit_json(changed=False, orbitron={"state": "absent", "revoked": False})

            if module.check_mode:
                module.exit_json(changed=True, orbitron={"state": "absent", "revoked": True})

            client.delete("/api/v1/tokens/" + token_value)
            module.exit_json(changed=True, orbitron={"state": "absent", "revoked": True})

        # state == present
        tokens = _list_tokens(client)
        existing = _find_by_label(tokens, label) if label else None

        if rotate:
            if not token_value:
                module.fail_json(msg="rotate=true requires token_value")
            if _find_by_value(tokens, token_value) is None:
                module.exit_json(changed=False, orbitron={"state": "present", "exists": False, "label": label})

            if module.check_mode:
                module.exit_json(changed=True, orbitron={"state": "present", "rotated": True, "label": label})

            status, body = client.post("/api/v1/tokens/" + token_value + "/rotate", payload=payload)
            module.exit_json(changed=True, orbitron=dict(body, state="present", rotated=True))

        if existing is not None:
            module.exit_json(changed=False, orbitron={"state": "present", "exists": True, "label": label})

        if module.check_mode:
            module.exit_json(changed=True, orbitron={"state": "present", "exists": False, "label": label})

        status, body = client.post("/api/v1/tokens", payload=payload)
        module.exit_json(changed=True, orbitron=dict(body, state="present", exists=False))
    except OrbitronError as exc:
        fail_orbitron(module, exc, "token operation")


if __name__ == "__main__":
    main()
