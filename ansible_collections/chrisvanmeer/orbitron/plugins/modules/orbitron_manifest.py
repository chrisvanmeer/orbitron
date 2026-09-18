#!/usr/bin/python
# -*- coding: utf-8 -*-
# Copyright (c) Ansible project contributors
# GNU General Public License v3.0+ (see LICENSES/GPL-3.0-or-later.txt or https://www.gnu.org/licenses/gpl-3.0.txt)
# SPDX-License-Identifier: GPL-3.0-or-later

DOCUMENTATION = r"""
---
module: orbitron_manifest
short_description: Store role/collection requirements manifests in Orbitron
version_added: "1.0.0"
description:
  - Stores a role or collection requirements manifest on an Orbitron mirror.
  - The module is idempotent by content hash. Submitting a manifest whose exact
    bytes are already stored reports changed=false and triggers nothing.
    A structurally different manifest replaces any stored manifest of the same
    type that declares overlapping names (server semantics).
  - The manifest may be supplied as raw C(content), as a local file via
    C(path), or as structured C(roles)/C(collections) lists.
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
  type:
    description: Type of requirement being declared.
    type: str
    choices: [roles, collections]
    required: true
  content:
    description:
      - The raw requirements YAML to submit verbatim.
    type: str
    required: false
  path:
    description:
      - Local path to a requirements file whose exact contents are submitted
        unmodified.
    type: path
    required: false
  roles:
    description:
      - Structured list of role requirements. Supported fields are name, src, scm,
        and version, mirroring a Galaxy requirements file.
    type: list
    elements: dict
    required: false
    suboptions:
      name:
        description: Role name (ns.name), or leave empty when src is enough.
        type: str
      src:
        description: Source of the role, typically a git URL.
        type: str
      scm:
        description: Source control mode, typically git.
        type: str
      version:
        description: Version, tag, branch, or specifier such as >=1.0.0.
        type: str
  collections:
    description:
      - Structured list of collection requirements.
    type: list
    elements: dict
    required: false
    suboptions:
      name:
        description: Collection full name (ns.name).
        type: str
      src:
        description: Collection source URL for git-based collections.
        type: str
      type:
        description: Collection type, e.g. git.
        type: str
      version:
        description: Version, tag, branch, or specifier such as >=2.0.0.
        type: str
      source:
        description: Source override for the collection.
        type: str
  validate_certs:
    description: Validate TLS certificates when C(url) is a https endpoint.
    type: bool
    default: false
  timeout:
    description: Timeout in seconds for each API call.
    type: int
    default: 10
notes:
  - Provide exactly one of C(content), C(path), or the matching structured
    list; providing several is an error.
  - The server queues a background sync of the submitted items; use
    M(chrisvanmeer.orbitron.orbitron_sync) afterwards to wait for completion.
"""

EXAMPLES = r"""
- name: Mirror two collections from structured lists
  chrisvanmeer.orbitron.orbitron_manifest:
    token: "{{ orbitron_admin_token }}"
    type: collections
    collections:
      - name: community.general
        version: "8.4.0"
      - name: community.docker
        version: "3.6.0"

- name: Mirror roles from raw content
  chrisvanmeer.orbitron.orbitron_manifest:
    token: "{{ orbitron_admin_token }}"
    type: roles
    content: |
      roles:
        - name: geerlingguy.nginx
          version: 3.3.1
"""

RETURN = r"""
orbitron:
  description: Result of the manifest submission.
  returned: success
  type: dict
  contains:
    type:
      description: types of the affected manifest.
      type: str
      sample: roles
    sha256:
      description: Content hash of the submitted manifest bytes.
      type: str
    submitted:
      description: Whether a manifest was newly stored (false when unchanged).
      type: bool
    stored:
      description: Whether the manifest is present on the server after the run.
      type: bool
"""

from ansible.module_utils.basic import AnsibleModule

from ansible_collections.chrisvanmeer.orbitron.plugins.module_utils.orbitron import (
    COMMON_ARG_SPEC,
    OrbitronClient,
    OrbitronError,
    fail_orbitron,
    resolve_manifest_body,
    sha256_text,
)


def _already_stored(manifests, kind, digest):
    entries = manifests.get(kind) or []
    return any((entry.get("sha256") or "") == digest for entry in entries)


def main():
    argument_spec = dict(
        type=dict(type="str", required=True, choices=["roles", "collections"]),
        content=dict(type="str", required=False),
        path=dict(type="path", required=False),
        roles=dict(
            type="list",
            elements="dict",
            required=False,
            options=dict(
                name=dict(type="str", required=False),
                src=dict(type="str", required=False),
                scm=dict(type="str", required=False),
                version=dict(type="str", required=False),
            ),
        ),
        collections=dict(
            type="list",
            elements="dict",
            required=False,
            options=dict(
                name=dict(type="str", required=False),
                src=dict(type="str", required=False),
                type=dict(type="str", required=False),
                version=dict(type="str", required=False),
                source=dict(type="str", required=False),
            ),
        ),
    )
    argument_spec.update(COMMON_ARG_SPEC)

    module = AnsibleModule(argument_spec=argument_spec, supports_check_mode=True)

    kind, body = resolve_manifest_body(module)
    digest = sha256_text(body)
    client = OrbitronClient(module)

    try:
        manifests = client.get("/api/v1/manifests")[1]
        if _already_stored(manifests, kind, digest):
            module.exit_json(changed=False, orbitron={"type": kind, "sha256": digest, "submitted": False, "stored": True})

        if module.check_mode:
            module.exit_json(changed=True, orbitron={"type": kind, "sha256": digest, "submitted": True, "stored": False})

        client.post("/api/v1/requirements/" + kind, raw_body=body)
        module.exit_json(changed=True, orbitron={"type": kind, "sha256": digest, "submitted": True, "stored": True})
    except OrbitronError as exc:
        fail_orbitron(module, exc, "manifest submission")


if __name__ == "__main__":
    main()
