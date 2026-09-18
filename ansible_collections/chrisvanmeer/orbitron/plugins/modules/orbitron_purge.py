#!/usr/bin/python
# -*- coding: utf-8 -*-
# Copyright (c) Ansible project contributors
# GNU General Public License v3.0+ (see LICENSES/GPL-3.0-or-later.txt or https://www.gnu.org/licenses/gpl-3.0.txt)
# SPDX-License-Identifier: GPL-3.0-or-later

DOCUMENTATION = r"""
---
module: orbitron_purge
short_description: Remove a cached role/collection version from Orbitron
version_added: "1.0.0"
description:
  - Deletes a single cached role or collection version from an Orbitron
    mirror.
  - Idempotent by inspection. When the version is not present in the storage
    inventory, changed=false and nothing is deleted. Deleting an exactly
    pinned version also removes the pin from the stored requirements manifest
    so a later sync does not silently re-fetch it.
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
  kind:
    description: Whether the version belongs to a role or a collection.
    type: str
    choices: [role, collection]
    required: true
  name:
    description:
      - Full identity of the cached item (namespace.name), e.g.
        geerlingguy.nginx or community.general.
    type: str
    required: true
  version:
    description: The exact cached version to remove.
    type: str
    required: true
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
- name: Drop a stale cached role version
  chrisvanmeer.orbitron.orbitron_purge:
    token: "{{ orbitron_admin_token }}"
    kind: role
    name: geerlingguy.nginx
    version: 1.2.2

- name: Drop an old collection artifact
  chrisvanmeer.orbitron.orbitron_purge:
    token: "{{ orbitron_admin_token }}"
    kind: collection
    name: community.general
    version: 8.3.0
"""

RETURN = r"""
orbitron:
  description: Result of the purge operation.
  returned: success
  type: dict
  contains:
    removed:
      description: Whether the version was actually deleted.
      type: bool
      sample: true
    remaining:
      description: Versions of the same item that remain cached.
      type: list
      elements: str
    item:
      description: Full identity of the purged item.
      type: str
"""

from ansible.module_utils.basic import AnsibleModule

from ansible_collections.chrisvanmeer.orbitron.plugins.module_utils.orbitron import (
    COMMON_ARG_SPEC,
    OrbitronClient,
    OrbitronError,
    fail_orbitron,
)


def _find_item(inventory, kind, name):
    entries = inventory.get("roles" if kind == "role" else "collections") or []
    for entry in entries:
        if entry.get("name") == name:
            return entry
    return None


def main():
    argument_spec = dict(
        kind=dict(type="str", required=True, choices=["role", "collection"]),
        name=dict(type="str", required=True),
        version=dict(type="str", required=True),
    )
    argument_spec.update(COMMON_ARG_SPEC)

    module = AnsibleModule(argument_spec=argument_spec, supports_check_mode=True)
    client = OrbitronClient(module)

    kind = module.params["kind"]
    name = module.params["name"]
    version = module.params["version"]

    try:
        inventory = client.get("/api/v1/storage")[1]
        item = _find_item(inventory, kind, name)

        remaining = [v["version"] for v in (item or {}).get("versions", [])]
        if item is None or version not in remaining:
            module.exit_json(
                changed=False,
                orbitron={"removed": False, "item": name, "remaining": remaining},
            )

        if module.check_mode:
            module.exit_json(
                changed=True,
                orbitron={"removed": True, "item": name, "remaining": [v for v in remaining if v != version]},
            )

        plural = "roles" if kind == "role" else "collections"
        client.delete("/api/v1/storage/" + plural + "/" + name + "/" + version)
        module.exit_json(
            changed=True,
            orbitron={
                "removed": True,
                "item": name,
                "version": version,
                "remaining": [v for v in remaining if v != version],
            },
        )
    except OrbitronError as exc:
        fail_orbitron(module, exc, "purge operation")


if __name__ == "__main__":
    main()
