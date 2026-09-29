#!/usr/bin/python
# -*- coding: utf-8 -*-
# Copyright (c) Ansible project contributors
# GNU General Public License v3.0+ (see LICENSES/GPL-3.0-or-later.txt or https://www.gnu.org/licenses/gpl-3.0.txt)
# SPDX-License-Identifier: GPL-3.0-or-later

DOCUMENTATION = r"""
---
module: orbitron_dump
short_description: Download the entire Orbitron cache as a .tar.gz backup archive
version_added: "2.1.0"
description:
  - Calls the Orbitron dump API and streams the whole mirrored content
    (roles, collection archives, git-sourced collection checkouts, stored
    requirements manifests and the access index) into a single gzip-compressed
    tar archive on the controller, ready to be shipped to a backup machine or
    sealed in an offline vault.
  - The archive carries a C(orbitron/dump.json) entry with the Orbitron version,
    a UTC timestamp, the cache inventory and a SHA-256 per file, so a receiving
    side can verify what it received.
  - The token store is never part of the dump. A content backup therefore never
    carries administrative credentials into the offline archive.
  - This task is not idempotent; it reports C(changed=true) on every run
    because a fresh snapshot is always written.
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
  dest:
    description:
      - Path on the controller where the C(.tar.gz) archive is written.
      - The parent directory is created when missing.
      - Written to C(<dest>.part) first and moved into place on success, so a
        failed transfer never leaves a partial archive behind.
    type: path
    required: true
  compress:
    description:
      - Compression level requested from the server.
      - V(default) favours archive size; V(fast) trades ratio for CPU, which
        helps when dumping a large mirror on a tight schedule.
    type: str
    choices: [default, fast]
    default: default
  read_timeout:
    description:
      - Socket timeout in seconds applied to the download.
      - Overrides the shared C(timeout) because a full-cache dump on a slow
        uplink regularly pauses longer than a normal API call.
    type: int
    default: 300
  validate_certs:
    description: Validate TLS certificates when C(url) is a https endpoint.
    type: bool
    default: false
  timeout:
    description: Timeout in seconds for the API call.
    type: int
    default: 10
notes:
  - Dumping while a background sync is running yields a consistent set of files
    but not a globally consistent one. The server reports this through the
    C(X-Orbitron-Dump-Sync) response header and the C(sync_running) field of
    C(orbitron/dump.json), and the per-file checksums make a torn dump
    detectable. Prefer dumping when no sync is in flight.
  - The download has no C(Content-Length), so a broken connection leaves a
    truncated archive rather than a partial one that resumes.
"""

EXAMPLES = r"""
- name: Take an offline backup of the whole cache
  chrisvanmeer.orbitron.orbitron_dump:
    token: "{{ orbitron_admin_token }}"
    dest: /var/backups/orbitron/orbitron-backup.tar.gz
  register: backup

- name: Ship the archive to the vault machine
  ansible.builtin.copy:
    src: "{{ backup.dest }}"
    dest: /mnt/offline-vault/orbitron/{{ backup.created_at }}.tar.gz
    mode: "0400"

- name: Dump a large mirror without burning CPU on compression
  chrisvanmeer.orbitron.orbitron_dump:
    token: "{{ orbitron_admin_token }}"
    dest: /var/backups/orbitron/nightly.tar.gz
    compress: fast
    read_timeout: 1800
"""

RETURN = r"""
dest:
  description: Path of the written archive.
  returned: success
  type: str
  sample: /var/backups/orbitron/orbitron-backup.tar.gz
size_bytes:
  description: Size of the downloaded archive in bytes.
  returned: success
  type: int
  sample: 9437184
sha256:
  description: SHA-256 of the downloaded archive, computed while streaming.
  returned: success
  type: str
  sample: 3f786850e387550fdab836ed7e6dc881de23001b
dump:
  description: The archive's own C(orbitron/dump.json) inventory.
  returned: success
  type: dict
  contains:
    created_at:
      description: UTC timestamp of the dump.
      type: str
      sample: "2026-09-29T10:15:00Z"
    orbitron_version:
      description: Version of the daemon that produced the archive.
      type: str
      sample: v13.1.0
    sync_running:
      description: Whether a background sync was running when the dump started.
      type: bool
      sample: false
    roles:
      description: Distinct cached role names in the archive.
      type: int
      sample: 12
    role_versions:
      description: Cached role versions in the archive.
      type: int
      sample: 30
    collections:
      description: Distinct cached collection names in the archive.
      type: int
      sample: 5
    collection_versions:
      description: Cached collection versions in the archive.
      type: int
      sample: 9
    excluded:
      description: Paths deliberately kept out of the archive.
      type: list
      elements: str
      sample: ["tokens.json", ".access.json.tmp"]
"""

import json
import os
import tarfile

from ansible.module_utils.basic import AnsibleModule

from ansible_collections.chrisvanmeer.orbitron.plugins.module_utils.orbitron import (
    COMMON_ARG_SPEC,
    OrbitronClient,
    OrbitronError,
)

# Path of the inventory entry inside the archive, kept in sync with the
# server's <dumpRoot>/dump.json.
MANIFEST_MEMBER = "orbitron/dump.json"

# Manifest fields surfaced to the playbook. The per-file checksum list is left
# out on purpose: it can hold thousands of entries.
SUMMARY_FIELDS = (
    "created_at",
    "orbitron_version",
    "sync_running",
    "roles",
    "role_versions",
    "collections",
    "collection_versions",
    "excluded",
)


def read_manifest(path):
    """Extract and parse the archive's dump.json, or return None when absent.

    A completed dump always carries the manifest; a truncated one does not, and
    that absence is itself the signal that the transfer broke.
    """
    try:
        with tarfile.open(path, "r:gz") as archive:
            member = archive.getmember(MANIFEST_MEMBER)
            handle = archive.extractfile(member)
            if handle is None:
                return None
            with handle:
                data = json.loads(handle.read().decode("utf-8"))
    except (tarfile.TarError, KeyError, ValueError, OSError):
        return None

    if not isinstance(data, dict):
        return None
    return dict((field, data.get(field)) for field in SUMMARY_FIELDS)


def main():
    argument_spec = dict(
        dest=dict(type="path", required=True),
        compress=dict(type="str", choices=["default", "fast"], default="default"),
        read_timeout=dict(type="int", default=300),
    )
    argument_spec.update(COMMON_ARG_SPEC)

    module = AnsibleModule(argument_spec=argument_spec, supports_check_mode=True)
    client = OrbitronClient(module)

    dest = module.params["dest"]
    query = "" if module.params["compress"] == "default" else "?compress=%s" % module.params["compress"]

    if module.check_mode:
        module.exit_json(
            changed=False,
            dest=dest,
            size_bytes=0,
            sha256="",
            dump={},
        )

    try:
        size, digest = client.download(
            "/api/v1/dump%s" % query,
            dest,
            timeout=module.params["read_timeout"],
        )
    except OrbitronError as exc:
        module.fail_json(msg="dump download failed: %s" % exc.message, status=exc.status, body=exc.body)

    if size == 0:
        module.fail_json(
            msg="dump download produced an empty archive at %s; the cache is most likely empty or the transfer was cut short" % dest,
            dest=dest,
        )

    manifest = read_manifest(dest)
    if manifest is None:
        module.fail_json(
            msg="archive at %s has no readable %s entry; the download is truncated or corrupt, retry the dump"
            % (dest, MANIFEST_MEMBER),
            dest=dest,
            size_bytes=size,
            sha256=digest,
        )

    module.exit_json(
        changed=True,
        dest=dest,
        size_bytes=size,
        sha256=digest,
        dump=manifest,
        # Convenience for chaining into a copy/upload task.
        dest_exists=os.path.isfile(dest),
    )


if __name__ == "__main__":
    main()
