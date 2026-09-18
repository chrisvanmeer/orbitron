# -*- coding: utf-8 -*-
"""Shared client and helpers for the chrisvanmeer.orbitron modules.

Every module talks to the Orbitron management REST API over HTTP. This module
covers the common transport (Bearer-token auth, HTTPS validation, error
mapping), content-addressing for manifests, and a tiny deterministic YAML
emitter so structured requirement lists become the exact manifest bytes the
server stores (no PyYAML dependency).
"""

import hashlib
import json
import os

from urllib.error import HTTPError

from ansible.module_utils.urls import open_url


class OrbitronError(Exception):
    """A failure talking to the Orbitron API, optionally carrying the HTTP
    status and response body that caused it."""

    def __init__(self, message, status=None, body=""):
        super(OrbitronError, self).__init__(message)
        self.message = message
        self.status = status
        self.body = body

    def __str__(self):
        if self.status is not None:
            return "%s (status=%s)" % (self.message, self.status)
        return self.message


# Argument spec shared by every HTTP-based orbitron module.
COMMON_ARG_SPEC = dict(
    url=dict(type="str", default="http://127.0.0.1:8080"),
    token=dict(type="str", required=False, no_log=True),
    validate_certs=dict(type="bool", default=False),
    timeout=dict(type="int", default=10),
)


def fail_orbitron(module, exc, action):
    """Fail the module with a normalized Orbitron error message."""
    if exc.status is not None:
        module.fail_json(
            msg="%s failed: %s" % (action, exc.message),
            status=exc.status,
            body=exc.body,
        )
    module.fail_json(msg="%s failed: %s" % (action, exc.message))


class OrbitronClient(object):
    """A small authenticated HTTP client for the Orbitron management API."""

    def __init__(self, module, token_required=True):
        url = module.params.get("url") or os.environ.get("ORBITRON_URL") or "http://127.0.0.1:8080"
        self.base_url = str(url).rstrip("/")
        self.module = module
        self.token = module.params.get("token") or os.environ.get("ORBITRON_TOKEN")
        self.validate_certs = bool(
            module.params.get("validate_certs") or self.base_url.startswith("https")
        )
        self.timeout = int(module.params.get("timeout") or 10)
        if token_required and not self.token:
            module.fail_json(msg="token is required (set the token parameter or ORBITRON_TOKEN)")

    def _headers(self):
        headers = {"Accept": "application/json"}
        if self.token:
            headers["Authorization"] = "Bearer %s" % self.token
        return headers

    def request(self, method, path, payload=None, raw_body=None):
        """Perform one HTTP request and return ``(status, json_dict)``.

        Non-2xx responses raise :class:`OrbitronError` with the status code so
        callers can implement idempotency against 404/501/401 semantics.
        """
        url = self.base_url + path
        headers = self._headers()
        data = None

        if raw_body is not None:
            headers["Content-Type"] = "text/yaml; charset=utf-8"
            data = raw_body
        elif payload is not None:
            headers["Content-Type"] = "application/json"
            data = json.dumps(payload)

        try:
            response = open_url(
                url,
                method=method,
                headers=headers,
                data=data,
                validate_certs=self.validate_certs,
                timeout=self.timeout,
            )
        except HTTPError as e:
            body = e.read().decode("utf-8", "replace") if hasattr(e, "read") else ""
            raise OrbitronError(
                "HTTP %s %s returned %d %s" % (method, path, e.code, e.reason),
                status=e.code,
                body=body,
            )
        except Exception as e:  # network / transport level failures
            raise OrbitronError("Failed to reach %s: %s" % (url, e))

        status = response.getcode()
        raw = response.read().decode("utf-8", "replace") if hasattr(response, "read") else ""
        decoded = {}
        if raw:
            try:
                decoded = json.loads(raw)
            except ValueError:
                decoded = {"raw": raw}
        return status, decoded

    def get(self, path, **kwargs):
        return self.request("GET", path, **kwargs)

    def post(self, path, payload=None, raw_body=None):
        return self.request("POST", path, payload=payload, raw_body=raw_body)

    def delete(self, path):
        return self.request("DELETE", path)


def sha256_text(text):
    """Return the hex digest of the exact manifest bytes the server stores."""
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def scalar(value):
    """Render a value as a deterministic single-quoted YAML scalar."""
    return "'%s'" % str(value).replace("'", "''")


def build_manifest(kind, items):
    """Serialize a structured list of role/collection requirements into the
    canonical ``roles:`` / ``collections:`` YAML the server accepts.

    The output is deterministic so its SHA-256 doubles as the content address
    the server reports back through ``GET /api/v1/manifests``.
    """
    out = ["%s:" % kind]
    for item in items:
        out.append("  - name: %s" % scalar(item.get("name") or ""))
        for key in ("src", "scm", "type", "version", "source"):
            value = item.get(key)
            if value is not None and str(value) != "":
                out.append("    %s: %s" % (key, scalar(value)))
    return "\n".join(out) + "\n"


def resolve_manifest_body(module):
    """Resolve the submitted manifest into ``(kind, body)`` from the mutually
    exclusive providers: ``content``, ``path``, or structured ``roles`` /
    ``collections`` lists."""
    kind = module.params.get("type")
    if kind not in ("roles", "collections"):
        module.fail_json(msg="type must be one of: roles, collections")

    content = module.params.get("content")
    path = module.params.get("path")
    roles = module.params.get("roles") or []
    collections = module.params.get("collections") or []

    providers = 0
    if content:
        providers += 1
    if path:
        providers += 1
    if kind == "roles" and roles:
        providers += 1
    if kind == "collections" and collections:
        providers += 1
    if providers > 1:
        module.fail_json(
            msg="provide exactly one of content, path, or a structured %s list" % kind
        )
    if providers == 0:
        module.fail_json(
            msg="no manifest content provided: use content, path, or a %s list" % kind
        )

    body = content
    if path:
        try:
            with open(path, "r", encoding="utf-8") as handle:
                body = handle.read()
        except IOError as e:
            module.fail_json(msg="cannot read manifest path %s: %s" % (path, e))
    if body is None:
        body = build_manifest(kind, roles if kind == "roles" else collections)

    return kind, str(body)
