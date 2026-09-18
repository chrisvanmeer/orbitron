# -*- coding: utf-8 -*-

from ansible_collections.chrisvanmeer.orbitron.plugins.modules import orbitron_manifest
from ansible_collections.chrisvanmeer.orbitron.plugins.modules import orbitron_purge
from ansible_collections.chrisvanmeer.orbitron.plugins.modules import orbitron_sync
from ansible_collections.chrisvanmeer.orbitron.plugins.modules import orbitron_token

BASE_PARAMS = {"url": "http://mirror.example", "token": "adm", "validate_certs": False, "timeout": 5}


class FakeAnsibleModule(object):
    """Replacement for AnsibleModule that records exit/fail calls."""

    def __init__(self, params, check_mode=False):
        self.params = params
        self.check_mode = check_mode
        self.result = {}
        self.failures = {}

    def fail_json(self, **kwargs):
        self.failures.update(kwargs)
        raise SystemExit(1)

    def exit_json(self, **kwargs):
        self.result.update(kwargs)
        raise SystemExit(0)


def _run(main):
    try:
        main()
    except SystemExit:
        pass


def _patch(monkeypatch, module_class, params, check_mode=False, client_impl=None):
    fake = FakeAnsibleModule(params, check_mode=check_mode)
    monkeypatch.setattr(module_class, "AnsibleModule", lambda **kw: fake)

    if client_impl is None:
        class _NoopClient(object):
            def __init__(self, module, token_required=True):
                self.fake = fake

            def get(self, path):
                return 200, {}

            def post(self, path, payload=None, raw_body=None):
                return 202, {"status": "ok"}

            def delete(self, path):
                return 200, {}

        client_impl = _NoopClient

    monkeypatch.setattr(module_class, "OrbitronClient", client_impl)
    return fake


# ---------------------------------------------------------------- manifest

def test_manifest_submits_when_not_stored(monkeypatch):
    class NoopClient(object):
        def __init__(self, module, token_required=True):
            pass

        def get(self, path):
            return 200, {"roles": [], "collections": []}

        def post(self, path, payload=None, raw_body=None):
            return 202, {"status": "roles_sync_started"}

    params = dict(BASE_PARAMS, type="roles", content="roles:\n", path=None, roles=[], collections=[])
    fake = _patch(monkeypatch, orbitron_manifest, params, client_impl=NoopClient)
    _run(orbitron_manifest.main)

    assert fake.result["changed"] is True
    assert fake.result["orbitron"]["submitted"] is True
    assert fake.result["orbitron"]["type"] == "roles"


def test_manifest_idempotent_on_matching_sha(monkeypatch):
    digest = orbitron_manifest.sha256_text("roles:\n  - name: 'geerlingguy.nginx'\n    version: '1.2.3'\n")

    class NoopClient(object):
        def __init__(self, module, token_required=True):
            pass

        def get(self, path):
            return 200, {"roles": [{"file": "roles_x_requirements.yml", "sha256": digest}], "collections": []}

    params = dict(
        BASE_PARAMS,
        type="roles",
        content=None,
        path=None,
        roles=[{"name": "geerlingguy.nginx", "version": "1.2.3"}],
        collections=[],
    )
    fake = _patch(monkeypatch, orbitron_manifest, params, client_impl=NoopClient)
    _run(orbitron_manifest.main)

    assert fake.result["changed"] is False
    assert fake.result["orbitron"]["submitted"] is False


def test_manifest_check_mode_reports_would_change(monkeypatch):
    class NoopClient(object):
        def __init__(self, module, token_required=True):
            pass

        def get(self, path):
            return 200, {"roles": [], "collections": []}

    params = dict(BASE_PARAMS, type="roles", content="roles:\n", path=None, roles=[], collections=[])
    fake = _patch(monkeypatch, orbitron_manifest, params, check_mode=True, client_impl=NoopClient)
    _run(orbitron_manifest.main)

    assert fake.result["changed"] is True
    assert fake.result["orbitron"]["stored"] is False


# ---------------------------------------------------------------- token

def test_token_creates_when_label_missing(monkeypatch):
    posts = []

    class NoopClient(object):
        def __init__(self, module, token_required=True):
            pass

        def get(self, path):
            return 200, {"tokens": [{"label": "other", "created_at": 1, "expires_at": 0}]}

        def post(self, path, payload=None):
            posts.append((path, payload))
            return 200, {"token": "abc123", "label": "ci", "created_at": 1, "expires_at": 0}

    params = dict(BASE_PARAMS, state="present", label="ci", ttl_days=30, token_value=None, rotate=False)
    fake = _patch(monkeypatch, orbitron_token, params, client_impl=NoopClient)
    _run(orbitron_token.main)

    assert fake.result["changed"] is True
    assert posts == [("/api/v1/tokens", {"label": "ci", "ttl_days": 30})]


def test_token_idempotent_by_label(monkeypatch):
    class NoopClient(object):
        def __init__(self, module, token_required=True):
            pass

        def get(self, path):
            return 200, {"tokens": [{"label": "ci", "created_at": 1, "expires_at": 0}]}

    params = dict(BASE_PARAMS, state="present", label="ci", ttl_days=None, token_value=None, rotate=False)
    fake = _patch(monkeypatch, orbitron_token, params, client_impl=NoopClient)
    _run(orbitron_token.main)

    assert fake.result["changed"] is False
    assert fake.result["orbitron"]["exists"] is True


def test_token_absent_revokes(monkeypatch):
    deletes = []

    class NoopClient(object):
        def __init__(self, module, token_required=True):
            pass

        def get(self, path):
            return 200, {"tokens": [{"token": "deadbeef", "label": "ci", "created_at": 1, "expires_at": 0}]}

        def delete(self, path):
            deletes.append(path)
            return 200, {"status": "revoked"}

    params = dict(BASE_PARAMS, state="absent", label=None, ttl_days=None, token_value="deadbeef", rotate=False)
    fake = _patch(monkeypatch, orbitron_token, params, client_impl=NoopClient)
    _run(orbitron_token.main)

    assert fake.result["changed"] is True
    assert deletes == ["/api/v1/tokens/deadbeef"]


def test_token_absent_noop_when_unknown(monkeypatch):
    class NoopClient(object):
        def __init__(self, module, token_required=True):
            pass

        def get(self, path):
            return 200, {"tokens": []}

    params = dict(BASE_PARAMS, state="absent", label=None, ttl_days=None, token_value="ghost", rotate=False)
    fake = _patch(monkeypatch, orbitron_token, params, client_impl=NoopClient)
    _run(orbitron_token.main)

    assert fake.result["changed"] is False
    assert fake.result["orbitron"]["revoked"] is False


# ---------------------------------------------------------------- sync

COLLECTION_META = {
    "type": "collections",
    "file": "collections_x_requirements.yml",
    "sha256": "digest",
    "collections": [{"Name": "community.general", "Version": "8.5.0"}],
}


class SyncClient(object):
    """Path-aware client: idle status, one declared collection, empty mirror."""

    def __init__(self, module, token_required=True):
        self.posted = []

    def get(self, path):
        if path == "/api/v1/sync/status":
            return 200, {"current": None, "history": []}
        if path == "/api/v1/manifests":
            return 200, {"roles": [], "collections": [COLLECTION_META]}
        return 200, {"roles": [], "collections": []}

    def post(self, path, payload=None):
        self.posted.append(path)
        return 202, {"status": "full_sync_triggered"}


def test_sync_triggers_when_pending(monkeypatch):
    params = dict(BASE_PARAMS, wait=True, timeout=5, skip_if_running=True, force=False)
    fake = _patch(monkeypatch, orbitron_sync, params, client_impl=SyncClient)
    _run(orbitron_sync.main)

    assert fake.result["changed"] is True
    assert fake.result["state"] == "completed"
    assert fake.result["pending"] == [{"kind": "collections", "name": "community.general", "version": "8.5.0"}]


def test_sync_idle_when_up_to_date(monkeypatch):
    class UpToDateClient(SyncClient):
        def get(self, path):
            if path == "/api/v1/storage":
                return 200, {
                    "roles": [],
                    "collections": [{"type": "collections", "name": "community.general", "versions": [{"version": "8.5.0"}]}],
                }
            return super(UpToDateClient, self).get(path)

    params = dict(BASE_PARAMS, wait=True, timeout=5, skip_if_running=True, force=False)
    fake = _patch(monkeypatch, orbitron_sync, params, client_impl=UpToDateClient)
    _run(orbitron_sync.main)

    assert fake.result["changed"] is False
    assert fake.result["state"] == "idle"
    assert fake.result["pending"] == []


def test_sync_running_reports_changed(monkeypatch):
    class RunningClient(object):
        posted = []

        def __init__(self, module, token_required=True):
            pass

        def get(self, path):
            return 200, {"current": {"id": "3", "kind": "full", "status": "running"}, "history": []}

        def post(self, path, payload=None):
            RunningClient.posted.append(path)
            return 202, {"status": "full_sync_triggered"}

    params = dict(BASE_PARAMS, wait=False, timeout=5, skip_if_running=True, force=False)
    fake = _patch(monkeypatch, orbitron_sync, params, client_impl=RunningClient)
    _run(orbitron_sync.main)

    assert fake.result["changed"] is True
    assert fake.result["state"] == "running"
    assert RunningClient.posted == []


def test_sync_running_with_wait_polls_to_completion(monkeypatch):
    class DrainingClient(object):
        def __init__(self, module, token_required=True):
            self.polls = 0

        def get(self, path):
            if path == "/api/v1/sync/status":
                self.polls += 1
                if self.polls == 1:
                    return 200, {"current": {"id": "3", "kind": "full", "status": "running"}, "history": []}
                return 200, {"current": None, "history": [{"id": "3", "status": "done"}]}
            if path == "/api/v1/manifests":
                return 200, {"roles": [], "collections": [COLLECTION_META]}
            return 200, {"roles": [], "collections": []}

        def post(self, path, payload=None):
            raise AssertionError("must not POST a second sync while one runs")

    params = dict(BASE_PARAMS, wait=True, timeout=5, skip_if_running=True, force=False)
    fake = _patch(monkeypatch, orbitron_sync, params, client_impl=DrainingClient)
    _run(orbitron_sync.main)

    assert fake.result["changed"] is True
    assert fake.result["state"] == "completed"


def test_sync_force_triggers_even_when_up_to_date(monkeypatch):
    class UpToDateClient(SyncClient):
        def get(self, path):
            if path == "/api/v1/storage":
                return 200, {
                    "roles": [],
                    "collections": [{"type": "collections", "name": "community.general", "versions": [{"version": "8.5.0"}]}],
                }
            return super(UpToDateClient, self).get(path)

    params = dict(BASE_PARAMS, wait=False, timeout=5, skip_if_running=True, force=True)
    fake = _patch(monkeypatch, orbitron_sync, params, client_impl=UpToDateClient)
    _run(orbitron_sync.main)

    assert fake.result["changed"] is True
    assert fake.result["state"] == "full_sync_triggered"


def test_sync_check_mode_reports_would_trigger(monkeypatch):
    class CheckModeClient(SyncClient):
        posted = []

        def post(self, path, payload=None):
            CheckModeClient.posted.append(path)
            return 202, {"status": "full_sync_triggered"}

    params = dict(BASE_PARAMS, wait=True, timeout=5, skip_if_running=True, force=False)
    fake = _patch(monkeypatch, orbitron_sync, params, check_mode=True, client_impl=CheckModeClient)
    _run(orbitron_sync.main)

    assert fake.result["changed"] is True
    assert fake.result["state"] == "would_trigger"
    assert CheckModeClient.posted == []


def test_plan_sync_pure_cases():
    manifests = {
        "roles": [
            {
                "roles": [
                    {"Name": "geerlingguy.nginx", "Version": "3.3.1"},
                    {"Name": "geerlingguy.nginx", "Version": "latest"},
                    {"Name": "geerlingguy.php", "Version": ">=2.0"},
                    {"Name": "geerlingguy.php", "Version": ""},
                    {"Name": "", "Src": "geerlingguy/ansible-role-nginx.git", "Version": "1.2.3"},
                ]
            }
        ],
        "collections": [
            {"collections": [{"Name": "community.general", "Version": "8.5.0"}, {"Name": "community.general", "Version": "v8.5.0"}]}
        ],
    }
    inventory = {
        "roles": [
            {
                "name": "geerlingguy.nginx",
                "versions": [{"version": "3.3.1"}, {"version": "1.2.3"}, {"version": "1.2.4"}],
            },
            {"name": "ansible-role-nginx", "versions": [{"version": "1.2.3"}]},
        ],
        "collections": [{"name": "community.general", "versions": [{"version": "8.5.0"}]}],
    }

    pending, should_sync = orbitron_sync.plan_sync(manifests, inventory, force=False)

    assert should_sync is True
    assert pending == [
        {"kind": "roles", "name": "geerlingguy.nginx", "version": "latest"},
        {"kind": "roles", "name": "geerlingguy.php", "version": ">=2.0"},
        {"kind": "roles", "name": "geerlingguy.php", "version": ""},
    ]

    pending, should_sync = orbitron_sync.plan_sync(manifests, inventory, force=True)
    assert pending == [
        {"kind": "roles", "name": "geerlingguy.nginx", "version": "latest"},
        {"kind": "roles", "name": "geerlingguy.php", "version": ">=2.0"},
        {"kind": "roles", "name": "geerlingguy.php", "version": ""},
    ]
    assert should_sync is True


def test_plan_sync_up_to_date_is_noop():
    manifests = {"roles": [], "collections": [{"collections": [{"Name": "community.general", "Version": "8.5.0"}]}]}
    inventory = {"collections": [{"name": "community.general", "versions": [{"version": "8.5.0"}]}]}

    pending, should_sync = orbitron_sync.plan_sync(manifests, inventory, force=False)

    assert pending == []
    assert should_sync is False


# ---------------------------------------------------------------- purge

def test_purge_removes_present_version(monkeypatch):
    deletes = []

    class NoopClient(object):
        def __init__(self, module, token_required=True):
            pass

        def get(self, path):
            return 200, {
                "roles": [{"name": "geerlingguy.nginx", "versions": [{"version": "1.2.2"}, {"version": "1.2.3"}]}],
                "collections": [],
            }

        def delete(self, path):
            deletes.append(path)
            return 200, {"status": "role_version_deleted"}

    params = dict(BASE_PARAMS, kind="role", name="geerlingguy.nginx", version="1.2.2")
    fake = _patch(monkeypatch, orbitron_purge, params, client_impl=NoopClient)
    _run(orbitron_purge.main)

    assert fake.result["changed"] is True
    assert deletes == ["/api/v1/storage/roles/geerlingguy.nginx/1.2.2"]
    assert fake.result["orbitron"]["remaining"] == ["1.2.3"]


def test_purge_noop_when_missing(monkeypatch):
    class NoopClient(object):
        def __init__(self, module, token_required=True):
            pass

        def get(self, path):
            return 200, {"roles": [{"name": "geerlingguy.nginx", "versions": [{"version": "1.2.3"}]}], "collections": []}

    params = dict(BASE_PARAMS, kind="role", name="geerlingguy.nginx", version="9.9.9")
    fake = _patch(monkeypatch, orbitron_purge, params, client_impl=NoopClient)
    _run(orbitron_purge.main)

    assert fake.result["changed"] is False
    assert fake.result["orbitron"]["removed"] is False
