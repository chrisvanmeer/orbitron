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
  - The module is idempotent. Before triggering anything it compares the
    declared requirements (C(GET /api/v1/manifests)) with what the mirror
    already stored (C(GET /api/v1/storage)). When every declared requirement is
    mirrored at its exact version the module reports changed=false and starts
    no new job. Versions that are empty, C(latest), or expression-style
    (e.g. C(>=2.0.0)) never match a stored version exactly, so they always
    warrant a re-check and trigger a sync.
  - Set C(force=true) to trigger a full re-sync even when the mirror already
    looks complete, and to always report changed=true.
  - When a sync is already running and C(skip_if_running=true) (the default),
    the module starts no second job. With C(wait=true) it waits for that job
    to settle and then reports changed only when the mirror actually gained
    new content; a rerun over an already-complete mirror is idempotent and
    reports changed=false. Without C(wait=true) it reports changed=true
    because sync work from this play is already in flight.
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
  force:
    description: Trigger a full re-sync even when everything declared appears mirrored.
    type: bool
    default: false
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
    description:
      - Do not start a new sync while one is already running.
      - With C(wait=true) the run then reports changed only when the in-flight
        job actually mirrored something new; without C(wait=true) it still
        reports changed=true since sync work from this play is already in
        flight.
    type: bool
    default: true
  validate_certs:
    description: Validate TLS certificates when C(url) is a https endpoint.
    type: bool
    default: false
notes:
  - Declared requirements come from the manifests the daemon stores via
    M(chrisvanmeer.orbitron.orbitron_manifest); the module never re-uploads
    them, it only asks the daemon to mirror what is declared.
"""

EXAMPLES = r"""
- name: Sync whenever declared content is missing, and wait for it
  chrisvanmeer.orbitron.orbitron_sync:
    token: "{{ orbitron_admin_token }}"
    wait: true

- name: Force a full re-sync regardless of the current mirror state
  chrisvanmeer.orbitron.orbitron_sync:
    token: "{{ orbitron_admin_token }}"
    force: true
"""

RETURN = r"""
pending:
  description:
    - Declared requirements that were not mirrored at their exact version when
      the module decided whether to sync. Empty when the sync was triggered by
      C(force) or suppressed because another job was already running.
  returned: success
  type: list
  elements: dict
state:
  description:
    - running, triggered, idle, or would_trigger.
    - completed when C(wait=true) observed the trigger job finishing.
  returned: success
  type: str
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
"""

import time

from ansible.module_utils.basic import AnsibleModule

from ansible_collections.chrisvanmeer.orbitron.plugins.module_utils.orbitron import (
    COMMON_ARG_SPEC,
    OrbitronClient,
    OrbitronError,
    fail_orbitron,
)


def _norm_version(version):
    """Normalize a declared/stored version for equality comparison: strips
    surrounding whitespace and quotes plus an optional leading ``v``."""
    value = str(version or "").strip().strip("\"'")
    if value.startswith("v"):
        value = value[1:]
    return value


def _declared_name(item):
    """Resolve the identity of a declared role/collection item the same way the
    daemon indexes it: the name, or the basename of src with .git stripped."""
    name = (item.get("Name") or "").strip().strip("\"'")
    if name:
        return name
    src = (item.get("Src") or "").strip()
    if not src:
        return ""
    src = src.rstrip("/")
    if src.endswith(".git"):
        src = src[:-4]
    for sep in ("/", ":"):
        if sep in src:
            src = src.rsplit(sep, 1)[-1]
    return src.strip("\"'")


def _mirrored_versions(inventory, kind, name):
    """Return the normalized versions the mirror already stored for a name."""
    for entry in inventory.get(kind) or []:
        if entry.get("name") == name:
            return [_norm_version(v.get("version")) for v in entry.get("versions") or []]
    return []


def _version_fingerprint(inventory):
    """Return a canonical, order-independent fingerprint of the mirrored
    inventory: every stored (name, version) pair for roles and collections.

    Used to detect whether a completed sync actually mirrored anything new,
    so a rerun over already-complete content reports C(changed=false).
    """
    pairs = set()
    for kind in ("roles", "collections"):
        for entry in inventory.get(kind) or []:
            name = (entry.get("name") or "").strip()
            if not name:
                continue
            for stored in entry.get("versions") or []:
                version = _norm_version(stored.get("version"))
                if version:
                    pairs.add((kind, name, version))
    return frozenset(pairs)


def plan_sync(manifests, inventory, force=False):
    """Decide whether a sync should run.

    Returns ``(pending, should_sync)`` where ``pending`` lists the declared
    requirements that are not mirrored at their exact version yet. Empty,
    ``latest``, and expression-style versions never match a stored version
    exactly and therefore always count as pending, so unpinned content is
    re-checked on every run.
    """
    pending = []

    for kind in ("roles", "collections"):
        for meta in manifests.get(kind) or []:
            for item in meta.get(kind) or []:
                name = _declared_name(item)
                if not name:
                    continue
                version = (item.get("Version") or "").strip().strip("\"'")
                key = _norm_version(version)
                if key and key in _mirrored_versions(inventory, kind, name):
                    continue
                pending.append({"kind": kind, "name": name, "version": version})

    return pending, force or bool(pending)


def wait_for_idle(client, module, deadline):
    """Poll /api/v1/sync/status until no job is running anymore.

    The poll interval backs off exponentially (1s → 2s → 4s → … up to 10s) so
    a long-running mirror job does not hammer the daemon's sync/status endpoint
    once per second and flood the access log."""

    start = time.time()
    delay = 1.0
    while True:
        snapshot = client.get("/api/v1/sync/status")[1]
        current_job = snapshot.get("current")
        if not current_job or current_job.get("status") != "running":
            return snapshot
        if time.time() - start > deadline:
            module.fail_json(
                msg="sync did not complete within %d seconds" % deadline,
                state="running",
                sync_status=snapshot,
            )
        module.sleep(min(delay, max(0.1, start + deadline - time.time())))
        delay = min(delay * 2, 10.0)


def main():
    argument_spec = dict(COMMON_ARG_SPEC)
    argument_spec.update(
        force=dict(type="bool", default=False),
        wait=dict(type="bool", default=True),
        timeout=dict(type="int", default=600),
        skip_if_running=dict(type="bool", default=True),
    )
    module = AnsibleModule(argument_spec=argument_spec, supports_check_mode=True)
    client = OrbitronClient(module)

    force = module.params["force"]
    wait = module.params["wait"]
    deadline = module.params["timeout"]
    skip_if_running = module.params["skip_if_running"]

    try:
        snapshot = client.get("/api/v1/sync/status")[1]
        current = snapshot.get("current") or {}
        running = current.get("status") == "running"

        manifests = client.get("/api/v1/manifests")[1]
        inventory = client.get("/api/v1/storage")[1]
        before_fp = _version_fingerprint(inventory)

        # A previous manifest submission already started a background sync.
        # Do not pile another full sync on top of the running one. When waiting
        # is requested, observe the outcome and only report the rerun as
        # changed when the in-flight job actually mirrored something new.
        if running and skip_if_running:
            state = "running"
            if wait:
                snapshot = wait_for_idle(client, module, deadline)
                state = "completed"
                after_inventory = client.get("/api/v1/storage")[1]
                changed = force or _version_fingerprint(after_inventory) != before_fp
            else:
                # Cannot observe the outcome without waiting; report the rerun
                # as changed because work is in flight from this play.
                changed = True
            module.exit_json(changed=changed, state=state, pending=[], sync_status=snapshot)

        pending, should_sync = plan_sync(manifests, inventory, force=force)

        if not (should_sync or (running and not skip_if_running)):
            module.exit_json(changed=False, state="idle", pending=[], sync_status=snapshot)

        if module.check_mode:
            module.exit_json(changed=True, state="would_trigger", pending=pending, sync_status=snapshot)

        body = client.post("/api/v1/sync")[1]
        state = body.get("status", "triggered")
        changed = True
        if wait:
            snapshot = wait_for_idle(client, module, deadline)
            state = "completed"
            after_inventory = client.get("/api/v1/storage")[1]
            # A sync that mirrored nothing new (every declared requirement was
            # already cached at its exact artifact) is an idempotent rerun and
            # must report ok, not changed. The daemon still re-checks the
            # declared requirements, so new upstream content is still picked up
            # on later runs without ever re-downloading what is already there.
            changed = force or _version_fingerprint(after_inventory) != before_fp

        module.exit_json(changed=changed, state=state, pending=pending, sync_status=snapshot)
    except OrbitronError as exc:
        fail_orbitron(module, exc, "sync operation")


if __name__ == "__main__":
    main()
