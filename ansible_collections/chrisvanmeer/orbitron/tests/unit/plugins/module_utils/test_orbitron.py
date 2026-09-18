# -*- coding: utf-8 -*-

import json
import urllib.error

import pytest

from ansible_collections.chrisvanmeer.orbitron.plugins.module_utils import orbitron as orbitron_utils


class _FakeModule(object):
    """Duck-typed substitute for AnsibleModule in pure unit tests."""

    def __init__(self, params):
        self.params = params

    def fail_json(self, **kwargs):
        raise AssertionError("fail_json called: %s" % kwargs)


class _FakeResponse(object):

    def __init__(self, status, payload=b"", as_text=None):
        self._status = status
        self._payload = payload
        self._text = as_text

    def getcode(self):
        return self._status

    def read(self):
        return self._payload if self._text is None else self._text.encode("utf-8")


def test_scalar_quotes_everything():
    assert orbitron_utils.scalar("community.general") == "'community.general'"
    assert orbitron_utils.scalar("a b") == "'a b'"
    assert orbitron_utils.scalar("it's") == "'it''s'"
    assert orbitron_utils.scalar(8) == "'8'"


def test_build_manifest_roles_canonical():
    body = orbitron_utils.build_manifest(
        "roles",
        [{"name": "geerlingguy.nginx", "version": "1.2.3", "src": "https://github.com/geerlingguy/ansible-role-nginx.git"}],
    )
    assert body == (
        "roles:\n"
        "  - name: 'geerlingguy.nginx'\n"
        "    src: 'https://github.com/geerlingguy/ansible-role-nginx.git'\n"
        "    version: '1.2.3'\n"
    )


def test_build_manifest_collections_fields():
    body = orbitron_utils.build_manifest(
        "collections",
        [{"name": "community.docker", "scm": "git", "type": "git", "source": "github"}],
    )
    assert body == (
        "collections:\n"
        "  - name: 'community.docker'\n"
        "    scm: 'git'\n"
        "    type: 'git'\n"
        "    source: 'github'\n"
    )


def test_sha256_text_known_value():
    assert orbitron_utils.sha256_text("collections:\n") == "737690a6973eaf2f40ad546a01472e2e10bc771b9b5d075d403dc7da194129ac"


def test_resolve_manifest_body_from_content():
    module = _FakeModule({"type": "roles", "content": "roles:\n", "path": None, "roles": [], "collections": []})
    kind, body = orbitron_utils.resolve_manifest_body(module)
    assert (kind, body) == ("roles", "roles:\n")


def test_resolve_manifest_body_from_path(tmp_path):
    req = tmp_path / "requirements.yml"
    req.write_text("collections:\n  - name: community.general\n", encoding="utf-8")
    module = _FakeModule({"type": "collections", "content": None, "path": str(req), "roles": [], "collections": []})
    kind, body = orbitron_utils.resolve_manifest_body(module)
    assert kind == "collections"
    assert body.startswith("collections:")


def test_resolve_manifest_body_from_lists():
    module = _FakeModule(
        {
            "type": "roles",
            "content": None,
            "path": None,
            "roles": [{"name": "geerlingguy.nginx", "version": "1.2.3"}],
            "collections": [],
        }
    )
    kind, body = orbitron_utils.resolve_manifest_body(module)
    assert kind == "roles"
    assert "geerlingguy.nginx" in body


def test_resolve_manifest_body_rejects_multiple_providers():
    module = _FakeModule(
        {
            "type": "roles",
            "content": "roles:\n",
            "path": None,
            "roles": [{"name": "geerlingguy.nginx"}],
            "collections": [],
        }
    )
    with pytest.raises(AssertionError):
        orbitron_utils.resolve_manifest_body(module)


def test_resolve_manifest_body_rejects_unknown_type():
    module = _FakeModule({"type": "users", "content": "x", "path": None, "roles": [], "collections": []})
    with pytest.raises(AssertionError):
        orbitron_utils.resolve_manifest_body(module)


class _CapturingClient(object):
    """OrbitronClient whose transport interactions are captured."""

    def __init__(self, module, token_required=True):
        self.cap = {"requests": []}
        self.token = module.params.get("token") or None
        horizon = orbitron_utils.OrbitronClient.__new__(orbitron_utils.OrbitronClient)
        horizon.base_url = "http://mirror.example"
        horizon.validate_certs = False
        horizon.timeout = 5
        horizon.token = self.token
        self._real = horizon

    def request(self, method, path, payload=None, raw_body=None):
        self.cap["requests"].append((method, path, payload, raw_body))
        return 200, {"status": "ok"}


def test_client_builds_bearer_and_json_post(monkeypatch):
    calls = {}

    def fake_open_url(url, method="GET", headers=None, data=None, validate_certs=None, timeout=None):
        calls.update(url=url, method=method, headers=headers, data=data)
        return _FakeResponse(200, json.dumps({"ok": True}).encode("utf-8"))

    monkeypatch.setattr(orbitron_utils, "open_url", fake_open_url)
    module = _FakeModule({"url": "http://mirror.example/", "token": "sekrit", "validate_certs": False, "timeout": 5})
    client = orbitron_utils.OrbitronClient(module)
    status, body = client.post("/api/v1/tokens", payload={"label": "ci"})

    assert calls["url"] == "http://mirror.example/api/v1/tokens"
    assert calls["method"] == "POST"
    assert calls["headers"]["Authorization"] == "Bearer sekrit"
    assert calls["headers"]["Content-Type"] == "application/json"
    assert json.loads(calls["data"]) == {"label": "ci"}
    assert (status, body) == (200, {"ok": True})


def test_client_submits_raw_yaml(monkeypatch):
    calls = {}

    def fake_open_url(url, method="GET", headers=None, data=None, validate_certs=None, timeout=None):
        calls.update(url=url, method=method, headers=headers, data=data)
        return _FakeResponse(202, b'{"status":"roles_sync_started"}')

    monkeypatch.setattr(orbitron_utils, "open_url", fake_open_url)
    module = _FakeModule({"url": "http://mirror.example", "token": "sekrit", "validate_certs": False, "timeout": 5})
    client = orbitron_utils.OrbitronClient(module)
    status, body = client.post("/api/v1/requirements/roles", raw_body="roles:\n")

    assert calls["headers"]["Content-Type"].startswith("text/yaml")
    assert calls["data"] == "roles:\n"
    assert status == 202


def test_client_maps_http_errors(monkeypatch):
    def fake_open_url(url, method="GET", headers=None, data=None, validate_certs=None, timeout=None):
        raise urllib.error.HTTPError(url, 501, "Not Implemented", None, None)

    monkeypatch.setattr(orbitron_utils, "open_url", fake_open_url)
    module = _FakeModule({"url": "http://mirror.example", "token": "sekrit", "validate_certs": False, "timeout": 5})
    client = orbitron_utils.OrbitronClient(module)

    with pytest.raises(orbitron_utils.OrbitronError) as excinfo:
        client.post("/api/v1/prune", payload={"dry_run": True})
    assert excinfo.value.status == 501


def test_client_allows_missing_token_for_healthz():
    module = _FakeModule({"url": "http://mirror.example", "token": None, "validate_certs": False, "timeout": 5})
    client = orbitron_utils.OrbitronClient(module, token_required=False)
    assert client.token is None
