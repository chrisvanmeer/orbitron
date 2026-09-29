# -*- coding: utf-8 -*-

import hashlib
import io
import json
import os
import tarfile

from ansible_collections.chrisvanmeer.orbitron.plugins.module_utils.orbitron import OrbitronError
from ansible_collections.chrisvanmeer.orbitron.plugins.modules import orbitron_dump

MANIFEST = {
    "created_at": "2026-09-29T10:15:00Z",
    "orbitron_version": "v13.1.0",
    "sync_running": False,
    "roles": 2,
    "role_versions": 3,
    "collections": 1,
    "collection_versions": 1,
    "excluded": ["tokens.json"],
    "files": [{"path": "roles/r/1.0.0", "sha256": "a" * 64}],
}


def build_archive(dest, manifest=None, with_manifest=True):
    """Write a realistic dump archive to dest and return (size, sha256)."""
    if manifest is None:
        manifest = MANIFEST
    buffer = io.BytesIO()
    with tarfile.open(fileobj=buffer, mode="w:gz") as archive:
        payload = b"role contents"
        info = tarfile.TarInfo("orbitron/roles/r/1.0.0/main.yml")
        info.size = len(payload)
        archive.addfile(info, io.BytesIO(payload))

        if with_manifest:
            body = json.dumps(manifest).encode("utf-8")
            info = tarfile.TarInfo("orbitron/dump.json")
            info.size = len(body)
            archive.addfile(info, io.BytesIO(body))

    raw = buffer.getvalue()
    with open(dest, "wb") as handle:
        handle.write(raw)
    return len(raw), hashlib.sha256(raw).hexdigest()


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


def _params(dest, **overrides):
    params = {
        "url": "http://mirror.example",
        "token": "adm",
        "validate_certs": False,
        "timeout": 5,
        "dest": dest,
        "compress": "default",
        "read_timeout": 300,
    }
    params.update(overrides)
    return params


def _patch(monkeypatch, dest, check_mode=False, download=None, **overrides):
    fake = FakeAnsibleModule(_params(dest, **overrides), check_mode=check_mode)
    monkeypatch.setattr(orbitron_dump, "AnsibleModule", lambda **kw: fake)

    calls = {}

    class _Client(object):
        def __init__(self, module, token_required=True):
            pass

        def download(self, path, target, timeout=None):
            calls.update(path=path, target=target, timeout=timeout)
            if download is not None:
                return download(path, target, timeout)
            return build_archive(target)

    monkeypatch.setattr(orbitron_dump, "OrbitronClient", _Client)
    return fake, calls


# ------------------------------------------------------------------- happy


def test_dump_writes_archive_and_returns_manifest(monkeypatch, tmp_path):
    dest = str(tmp_path / "backup.tar.gz")
    fake, calls = _patch(monkeypatch, dest)
    _run(orbitron_dump.main)

    assert fake.failures == {}
    assert fake.result["changed"] is True
    assert fake.result["dest"] == dest
    assert os.path.isfile(dest)
    assert calls["path"] == "/api/v1/dump"
    assert calls["timeout"] == 300
    assert calls["target"] == dest

    raw = open(dest, "rb").read()
    assert fake.result["size_bytes"] == len(raw)
    assert fake.result["sha256"] == hashlib.sha256(raw).hexdigest()

    # The per-file checksum list is intentionally not surfaced; it can be huge.
    assert "files" not in fake.result["dump"]
    assert fake.result["dump"]["orbitron_version"] == "v13.1.0"
    assert fake.result["dump"]["excluded"] == ["tokens.json"]
    assert fake.result["dump"]["sync_running"] is False


def test_dump_fast_compression_appends_query(monkeypatch, tmp_path):
    dest = str(tmp_path / "backup.tar.gz")
    fake, calls = _patch(monkeypatch, dest, compress="fast")
    _run(orbitron_dump.main)

    assert fake.failures == {}
    assert calls["path"] == "/api/v1/dump?compress=fast"


def test_dump_default_compression_has_no_query(monkeypatch, tmp_path):
    dest = str(tmp_path / "backup.tar.gz")
    _fake, calls = _patch(monkeypatch, dest)
    _run(orbitron_dump.main)
    assert calls["path"] == "/api/v1/dump"


def test_dump_honours_read_timeout(monkeypatch, tmp_path):
    dest = str(tmp_path / "backup.tar.gz")
    _fake, calls = _patch(monkeypatch, dest, read_timeout=1800)
    _run(orbitron_dump.main)
    assert calls["timeout"] == 1800


def test_dump_check_mode_does_not_download(monkeypatch, tmp_path):
    dest = str(tmp_path / "backup.tar.gz")
    fake, calls = _patch(monkeypatch, dest, check_mode=True)
    _run(orbitron_dump.main)

    assert fake.failures == {}
    assert fake.result["changed"] is False
    assert calls == {}
    assert not os.path.exists(dest)


# ----------------------------------------------------------------- failure


def test_dump_fails_when_archive_has_no_manifest(monkeypatch, tmp_path):
    dest = str(tmp_path / "backup.tar.gz")
    fake, _calls = _patch(
        monkeypatch,
        dest,
        download=lambda p, target, t: build_archive(target, with_manifest=False),
    )
    _run(orbitron_dump.main)

    assert "truncated or corrupt" in fake.failures["msg"]
    assert fake.failures["dest"] == dest
    # The digest is still reported so an operator can compare archives.
    assert len(fake.failures["sha256"]) == 64


def test_dump_fails_on_empty_archive(monkeypatch, tmp_path):
    dest = str(tmp_path / "backup.tar.gz")

    def _empty(path, target, timeout):
        open(target, "wb").close()
        return 0, ""

    fake, _calls = _patch(monkeypatch, dest, download=_empty)
    _run(orbitron_dump.main)

    assert "empty archive" in fake.failures["msg"]


def test_dump_fails_on_client_error(monkeypatch, tmp_path):
    dest = str(tmp_path / "backup.tar.gz")

    def _raise(path, target, timeout):
        raise OrbitronError("HTTP GET /api/v1/dump returned 401 Unauthorized", status=401, body="nope")

    fake, _calls = _patch(monkeypatch, dest, download=_raise)
    _run(orbitron_dump.main)

    assert "dump download failed" in fake.failures["msg"]
    assert fake.failures["status"] == 401
    assert fake.failures["body"] == "nope"


# ------------------------------------------------------------ read_manifest


def test_read_manifest_returns_none_for_garbage(tmp_path):
    broken = tmp_path / "broken.tar.gz"
    broken.write_bytes(b"this is not a tar archive")
    assert orbitron_dump.read_manifest(str(broken)) is None


def test_read_manifest_returns_none_for_missing_file(tmp_path):
    assert orbitron_dump.read_manifest(str(tmp_path / "absent.tar.gz")) is None


def test_read_manifest_ignores_unexpected_shape(tmp_path):
    dest = str(tmp_path / "list.tar.gz")
    build_archive(dest, manifest=["not", "a", "dict"])
    assert orbitron_dump.read_manifest(dest) is None
