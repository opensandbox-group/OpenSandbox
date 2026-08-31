# Copyright 2026 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# OpenSandbox egress system addon.
#
# Always loaded by the egress mitmproxy launcher. Stays transparent on the
# wire (does not add or alter headers that would reveal the proxy to peers).
#
# Behavior:
#   1. Forces streaming for SSE / chunked responses so each chunk is forwarded
#      immediately, bypassing the stream_large_bodies=1m buffer set in config.yaml
#      (which otherwise stalls LLM-style small-chunk streams).
#   2. Acts as Credential Proxy when the egress sidecar has an active
#      Credential Vault revision, read from the Go sidecar over a private Unix
#      socket. Credential values are never logged; response header values
#      containing them are redacted. Response bodies are not rewritten.
#      Processing is split across hooks because stream_large_bodies=1m streams
#      bodies above 1 MiB upstream before the `request` hook fires: binding
#      match, path/query/header rewrites and header injection run in
#      `requestheaders` (before the upstream connection is made); only body
#      substitutions, which need the full body, run in `request` and are
#      skipped for streamed requests.
#   3. Implements SNI-aware ignore_hosts for transparent mode. mitmproxy's
#      built-in ignore_hosts check in transparent mode matches against the
#      destination IP first; the SNI hostname is only available inside the TLS
#      ClientHello, which arrives after the initial check. This addon re-checks
#      the same ignore_hosts patterns against the SNI hostname at the
#      tls_clienthello layer and sets ignore_connection=True when a match is
#      found, ensuring domain-based TLS pass-through works reliably.
#   4. Passes through TLS connections that carry no SNI. Without a hostname,
#      upstream hostname verification falls back to the destination IP, which
#      fails for any public certificate lacking an IP SAN (hostname mismatch),
#      so every no-SNI connection would otherwise become a broken MITM attempt.
#      Pass-through is skipped when ssl_insecure is enabled, keeping the
#      explicit insecure-MITM escape hatch working for no-SNI clients.
#      TCP-layer enforcement (deny/allow rules) still applies to these flows.
#
# User-defined addons can be loaded alongside this script via
# OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT (comma-separated for multiple scripts).
from __future__ import annotations

import http.client as http_client
import json
import os
import re
import socket
import time
from typing import Any
from urllib.parse import quote, quote_plus, unquote

from mitmproxy import ctx, http
from mitmproxy.tls import ClientHelloData


CREDENTIAL_PROXY_SOCKET_ENV = "OPENSANDBOX_CREDENTIAL_PROXY_SOCKET"
DEFAULT_CREDENTIAL_PROXY_SOCKET = "/run/opensandbox/credential-proxy/active.sock"
ACTIVE_VAULT_PATH = "/credential-vault/_active"
VAULT_CACHE_TTL_SECONDS = 0.5
FLOW_REDACTIONS_KEY = "opensandbox_credential_redactions"
FLOW_BINDING_KEY = "opensandbox_credential_binding"
FLOW_VAULT_REDACTIONS_KEY = "opensandbox_credential_vault_redactions"
HEADER_SUBSTITUTION_DENYLIST = {
    "host",
    "content-length",
    "transfer-encoding",
    "connection",
    "upgrade",
    "te",
    "trailer",
    "proxy-authorization",
    "proxy-authenticate",
    "forwarded",
    "x-forwarded-for",
    "x-forwarded-host",
    "x-forwarded-proto",
}


class ActiveVault:
    def __init__(
        self,
        revision: int,
        bindings: list[dict[str, Any]],
        redactions: list[str],
    ) -> None:
        self.revision = revision
        self.bindings = bindings
        self.redactions = redactions


_vault_cache: ActiveVault | None = None
_vault_cache_loaded_at = 0.0

# Fleet profile: one shared mitmdump serving N sandboxes; the active vault is
# selected by the client's source IP (preserved by the interception DNAT), so
# the 0.5s cache is keyed per client IP. The sidecar profile keeps the single
# shared cache and never uses these. The per-IP cache is bounded: legitimate
# entries are bounded by the sandbox count, but spoofed source IPs could grow
# it without limit, so it is dropped wholesale past the cap.
_fleet_mode_enabled = False
_vault_cache_by_ip: dict[str, ActiveVault | None] = {}
_vault_cache_by_ip_loaded_at: dict[str, float] = {}
_VAULT_CACHE_MAX_IPS = 4096


def _set_fleet_mode(enabled: bool) -> None:
    global _fleet_mode_enabled
    _fleet_mode_enabled = enabled


def _set_fleet_mode_from_env() -> None:
    _set_fleet_mode(os.environ.get("OPENSANDBOX_EGRESS_PROFILE", "").strip().lower() == "fleet")


_set_fleet_mode_from_env()


class UnixSocketHTTPConnection(http_client.HTTPConnection):
    def __init__(self, socket_path: str, timeout: float) -> None:
        super().__init__("credential-proxy", timeout=timeout)
        self.socket_path = socket_path

    def connect(self) -> None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(self.timeout)
        sock.connect(self.socket_path)
        self.sock = sock


def tls_clienthello(data: ClientHelloData) -> None:
    """Re-check ignore_hosts patterns against SNI hostname.

    In transparent mode, mitmproxy checks ignore_hosts against the
    destination IP:port before the TLS handshake.  If the check fails at
    that stage (SNI not yet available), we get a second chance here with
    the actual hostname from the ClientHello SNI extension.

    Connections without SNI cannot be matched against hostname patterns and
    cannot be safely MITM'd: with no hostname available, upstream hostname
    verification falls back to the destination IP, which fails for any public
    certificate without an IP SAN (hostname mismatch), turning every no-SNI
    connection into a broken MITM attempt. Such connections are passed through
    untouched, unless the operator explicitly opted into insecure upstream
    verification (OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE), in which case
    MITM remains possible and the escape hatch keeps working. TCP-layer
    enforcement (deny/allow rules) still applies.
    """
    sni = data.client_hello.sni
    if not sni:
        if not ctx.options.ssl_insecure:
            data.ignore_connection = True
        return

    patterns = ctx.options.ignore_hosts
    if not patterns:
        return

    for pattern in patterns:
        try:
            if re.search(pattern, sni):
                data.ignore_connection = True
                return
        except re.error:
            pass


def _load_active_vault(client_ip: str | None = None) -> ActiveVault | None:
    if _fleet_mode_enabled:
        return _load_active_vault_for_ip(client_ip)
    return _load_active_vault_shared()


def _load_active_vault_shared() -> ActiveVault | None:
    global _vault_cache, _vault_cache_loaded_at
    now = time.monotonic()
    if _vault_cache is not None and now - _vault_cache_loaded_at < VAULT_CACHE_TTL_SECONDS:
        return _vault_cache

    vault = _fetch_active_vault(None)
    _vault_cache = vault
    _vault_cache_loaded_at = now
    return vault


def _load_active_vault_for_ip(client_ip: str | None) -> ActiveVault | None:
    if not client_ip:
        return None
    global _vault_cache_by_ip, _vault_cache_by_ip_loaded_at
    now = time.monotonic()
    if len(_vault_cache_by_ip) >= _VAULT_CACHE_MAX_IPS:
        _vault_cache_by_ip.clear()
        _vault_cache_by_ip_loaded_at.clear()
    if client_ip in _vault_cache_by_ip and now - _vault_cache_by_ip_loaded_at[client_ip] < VAULT_CACHE_TTL_SECONDS:
        return _vault_cache_by_ip[client_ip]

    vault = _fetch_active_vault(client_ip)
    _vault_cache_by_ip[client_ip] = vault
    _vault_cache_by_ip_loaded_at[client_ip] = now
    return vault


def _fetch_active_vault(client_ip: str | None = None) -> ActiveVault | None:
    socket_path = (
        os.environ.get(CREDENTIAL_PROXY_SOCKET_ENV, "").strip()
        or DEFAULT_CREDENTIAL_PROXY_SOCKET
    )
    path = ACTIVE_VAULT_PATH
    if client_ip:
        # fleet profile: one shared socket, dispatch inside — the handler
        # resolves clientIp -> subject -> that subject's vault snapshot
        path = f"{ACTIVE_VAULT_PATH}?clientIp={quote(client_ip)}"
    connection = UnixSocketHTTPConnection(socket_path, timeout=0.25)
    try:
        connection.request("GET", path)
        response = connection.getresponse()
        body = response.read()
        if response.status == 404:
            if _fleet_mode_enabled and client_ip:
                # an unknown source IP is an anomaly in the fleet profile
                # (every sandbox is a registered subject with a vault by
                # design); record it instead of failing silently
                ctx.log.warn(
                    f"credential proxy: no subject for clientIp {client_ip} "
                    "(fleet dispatch miss; no credentials injected)"
                )
            return None
        if response.status != 200:
            ctx.log.warn(
                f"credential proxy: active vault lookup failed with HTTP {response.status}"
            )
            return None
        payload = json.loads(body.decode("utf-8"))
    except Exception as exc:  # noqa: BLE001 - mitm addon must not crash traffic handling
        ctx.log.warn(f"credential proxy: active vault lookup failed: {exc}")
        return None
    finally:
        connection.close()

    bindings = payload.get("bindings") or []
    redactions = [v for v in (payload.get("redactions") or []) if isinstance(v, str) and v]
    return ActiveVault(
        revision=int(payload.get("revision") or 0),
        bindings=bindings,
        redactions=redactions,
    )


def _request_host(flow: http.HTTPFlow) -> str:
    host = flow.request.pretty_host or flow.request.host or ""
    return host.rstrip(".").lower()


def _request_port(flow: http.HTTPFlow) -> int:
    if flow.request.port:
        return int(flow.request.port)
    return 443 if flow.request.scheme == "https" else 80


def _request_path(flow: http.HTTPFlow) -> str:
    path = flow.request.path or "/"
    return path.split("?", 1)[0] or "/"


_DOT_SEGMENT_RE = re.compile(r"/\.\.(/|$)")


def _path_is_ambiguous(raw_path: str, *, allow_single_encoded_slash: bool = False) -> bool:
    """Return True if the raw request path could decode to a different path
    than the one used for binding match (dot-segments, encoded separators).
    Legitimate clients resolve dot segments before sending, so ``..`` on the
    wire is an attempt to confuse path-based authorization.

    ``allow_single_encoded_slash`` tolerates a single-layer ``%2f`` (legit
    for npm scoped package registry paths like ``/@scope%2fname``) on the
    raw wire path; nested encodings, backslashes and dot-segments are always
    rejected. The complementary
    :func:`_path_encoded_slash_changes_binding` check rejects a ``%2f`` that
    would cross an authorization boundary.
    """
    path = raw_path.split("?", 1)[0]

    # Only match ``..`` as a complete path segment (/../ or trailing /..).
    if _DOT_SEGMENT_RE.search(path):
        return True

    # Iteratively decode to catch nested encodings like %252e%252e or %252f.
    decoded = path
    for _ in range(10):
        lower = decoded.lower()
        if "%2f" in lower:
            # Tolerate a single-layer ``%2f`` on the first pass only; a nested
            # ``%252f`` decodes back to ``%2f`` and still trips this check.
            if not (allow_single_encoded_slash and decoded is path):
                return True
        if "%5c" in lower:
            return True
        if "\\" in decoded:
            return True
        next_decoded = unquote(decoded)
        if next_decoded == decoded:
            break
        decoded = next_decoded
    if _DOT_SEGMENT_RE.search(decoded):
        return True

    return False


def _path_encoded_slash_changes_binding(
    flow: http.HTTPFlow, vault: ActiveVault
) -> bool:
    """Return True if decoding ``%2f`` in the raw path would change which
    credential binding matches (i.e. the encoded slash crosses an
    authorization boundary). Legit uses like npm scoped packages decode to a
    path matching the same binding, so they pass; crafted paths like
    ``/api/v8/projects/123%2f..%2f456/variables`` are rejected before
    credential injection.
    """
    raw_path = _request_path(flow)
    if "%2f" not in raw_path.lower():
        return False

    decoded_path = unquote(raw_path)
    if decoded_path == raw_path:
        return False

    # If the decoded form contains dot-segments, treat it as ambiguous.
    if _DOT_SEGMENT_RE.search(decoded_path):
        return True

    scheme = (flow.request.scheme or "").lower()
    host = _request_host(flow)
    port = _request_port(flow)
    method = (flow.request.method or "").upper()

    def _non_path_matches(binding: dict[str, Any]) -> bool:
        match = binding.get("match") or {}
        schemes = match.get("schemes") or ["https"]
        if scheme not in schemes:
            return False
        canonical_port = 443 if scheme == "https" else 80
        if port != canonical_port:
            return False
        methods = [m.upper() for m in (match.get("methods") or ["GET", "POST", "PUT", "PATCH", "DELETE"])]
        if method not in methods:
            return False
        for pattern in match.get("hosts") or []:
            ok, _ = _host_matches(host, pattern)
            if ok:
                return True
        return False

    def _matches_with_path(path: str) -> set[int]:
        matched: set[int] = set()
        for idx, binding in enumerate(vault.bindings):
            if not _non_path_matches(binding):
                continue
            paths = (binding.get("match") or {}).get("paths") or ["/*"]
            if any(_path_matches(path, p) for p in paths):
                matched.add(idx)
        return matched

    return _matches_with_path(raw_path) != _matches_with_path(decoded_path)


def _host_matches(host: str, pattern: str) -> tuple[bool, int]:
    pattern = pattern.rstrip(".").lower()
    if pattern.startswith("*."):
        suffix = pattern[1:]
        apex = pattern[2:]
        return host.endswith(suffix) and host != apex, 1
    return host == pattern, 2


def _path_matches(path: str, pattern: str) -> bool:
    if pattern.endswith("*"):
        return path.startswith(pattern[:-1])
    return path == pattern


def _binding_matches(flow: http.HTTPFlow, binding: dict[str, Any]) -> tuple[bool, int]:
    match = binding.get("match") or {}
    scheme = (flow.request.scheme or "").lower()
    host = _request_host(flow)
    port = _request_port(flow)
    method = (flow.request.method or "").upper()
    path = _request_path(flow)

    schemes = match.get("schemes") or ["https"]
    if scheme not in schemes:
        return False, 0
    canonical_port = 443 if scheme == "https" else 80
    if port != canonical_port:
        return False, 0
    if method not in [m.upper() for m in (match.get("methods") or ["GET", "POST", "PUT", "PATCH", "DELETE"])]:
        return False, 0
    if not any(_path_matches(path, p) for p in (match.get("paths") or ["/*"])):
        return False, 0

    best_precedence = 0
    for pattern in match.get("hosts") or []:
        ok, precedence = _host_matches(host, pattern)
        if ok and precedence > best_precedence:
            best_precedence = precedence
    return best_precedence > 0, best_precedence


def _request_may_be_streamed(flow: http.HTTPFlow) -> bool:
    """True if mitmproxy may enable request-body streaming for this flow.

    Streaming is enabled whenever the body is expected to exceed
    ``stream_large_bodies`` (1 MiB), either up front (known Content-Length)
    or mid-upload once buffered bytes cross the threshold (chunked or
    HTTP/2 bodies without Content-Length). A 403 response cannot be served
    for such flows (mitmproxy 11.0.2 raises ``NotImplementedError``), so
    they must be killed instead.
    """
    if getattr(flow.request, "stream", False):
        return True
    if "content-length" in flow.request.headers:
        return False
    if (flow.request.http_version or "").upper().startswith("HTTP/2"):
        return True
    return "transfer-encoding" in flow.request.headers


def _reject_request(flow: http.HTTPFlow, body: bytes) -> None:
    """Terminate a request before it is forwarded upstream.

    mitmproxy 11.0.2 refuses to serve a locally-set response while a request
    body is being streamed: ``start_request_stream`` raises
    ``NotImplementedError`` once ``flow.response`` is set, and streaming is
    enabled as soon as a body is known to exceed ``stream_large_bodies``
    (1 MiB) — either up front via Content-Length or mid-upload for chunked
    bodies. A 403 response is therefore only safe when the body size is
    fully known; otherwise the flow is killed, which closes the client
    connection without forwarding anything.
    """
    if _request_may_be_streamed(flow):
        if flow.killable:
            flow.kill()
        return
    flow.response = http.Response.make(403, body, {"content-type": "text/plain"})


def _flow_rejected(flow: http.HTTPFlow) -> bool:
    """True if the flow was terminated by :func:`_reject_request` (403
    response or killed flow)."""
    if flow.error is not None:
        return True
    return flow.response is not None and getattr(flow.response, "status_code", None) == 403


def _select_binding(flow: http.HTTPFlow, vault: ActiveVault) -> dict[str, Any] | None:
    matches: list[tuple[int, dict[str, Any]]] = []
    for binding in vault.bindings:
        ok, precedence = _binding_matches(flow, binding)
        if ok:
            matches.append((precedence, binding))
    if not matches:
        return None

    highest = max(precedence for precedence, _ in matches)
    selected = [binding for precedence, binding in matches if precedence == highest]
    if len(selected) != 1:
        _reject_request(flow, b"credential binding ambiguous\n")
        ctx.log.warn(
            "credential proxy: ambiguous binding match for "
            f"{flow.request.method} {_request_host(flow)}{_request_path(flow)}"
        )
        return None
    return selected[0]


def _split_path_query(raw_path: str) -> tuple[str, str | None]:
    if "?" not in raw_path:
        return raw_path or "/", None
    path, query = raw_path.split("?", 1)
    return path or "/", query


def _request_body_bytes(flow: http.HTTPFlow) -> bytes | None:
    body = getattr(flow.request, "raw_content", None)
    if body is None:
        body = getattr(flow.request, "content", None)
    if body is None:
        return None
    if isinstance(body, str):
        return body.encode("utf-8")
    return body


def _set_request_body_bytes(flow: http.HTTPFlow, body: bytes) -> None:
    flow.request.content = body
    if "transfer-encoding" in flow.request.headers:
        del flow.request.headers["transfer-encoding"]
    flow.request.headers["content-length"] = str(len(body))


def _encoded_substitution_value(value: str, surface: str, content_type: str = "") -> str:
    if surface in {"path", "query"}:
        return quote(value, safe="")
    if surface == "body":
        normalized_type = content_type.split(";", 1)[0].strip().lower()
        if normalized_type == "application/json" or normalized_type.endswith("+json"):
            return json.dumps(value)[1:-1]
        if normalized_type == "application/x-www-form-urlencoded":
            return quote_plus(value, safe="")
    return value


def _replace_literals_once(text: str, replacements: list[tuple[str, str]]) -> tuple[str, list[int]]:
    # Apply replacements against the original text so inserted credential values
    # are never scanned again for later placeholders.
    if not replacements:
        return text, []

    parts: list[str] = []
    applied: list[int] = []
    applied_set: set[int] = set()
    i = 0
    while i < len(text):
        selected_index = -1
        selected_placeholder = ""
        selected_replacement = ""
        for index, (placeholder, replacement) in enumerate(replacements):
            if len(placeholder) <= len(selected_placeholder):
                continue
            if text.startswith(placeholder, i):
                selected_index = index
                selected_placeholder = placeholder
                selected_replacement = replacement
        if selected_index >= 0:
            parts.append(selected_replacement)
            if selected_index not in applied_set:
                applied.append(selected_index)
                applied_set.add(selected_index)
            i += len(selected_placeholder)
            continue
        parts.append(text[i])
        i += 1

    if not applied:
        return text, []
    return "".join(parts), applied


def _substitution_replacements(
    substitutions: list[dict[str, Any]], surface: str, content_type: str = ""
) -> list[tuple[str, str]]:
    replacements: list[tuple[str, str]] = []
    for substitution in substitutions:
        placeholder = substitution.get("placeholder")
        value = substitution.get("value")
        surfaces = substitution.get("in") or []
        if not placeholder or value is None or surface not in surfaces:
            continue
        replacements.append(
            (
                str(placeholder),
                _encoded_substitution_value(str(value), surface, content_type),
            )
        )
    return replacements


def _apply_path_query_substitutions(
    flow: http.HTTPFlow,
    substitutions: list[dict[str, Any]],
) -> list[str]:
    raw_path = flow.request.path or "/"
    path_part, query_part = _split_path_query(raw_path)
    path_changed = False
    query_changed = False
    applied: list[str] = []

    path_part, path_applied = _replace_literals_once(
        path_part, _substitution_replacements(substitutions, "path")
    )
    if path_applied:
        path_changed = True
        applied.extend("path" for _ in path_applied)
    if query_part is not None:
        query_part, query_applied = _replace_literals_once(
            query_part, _substitution_replacements(substitutions, "query")
        )
        if query_applied:
            query_changed = True
            applied.extend("query" for _ in query_applied)

    if path_changed:
        candidate_path = path_part
        if query_part is not None:
            candidate_path = f"{candidate_path}?{query_part}"
        if _path_is_ambiguous(candidate_path):
            _reject_request(
                flow, b"request path contains ambiguous substituted segments\n"
            )
            ctx.log.warn(
                "credential proxy: rejected request after path substitution: "
                f"{flow.request.method} {_request_host(flow)} path=[REDACTED]"
            )
            return applied

    if path_changed or query_changed:
        flow.request.path = path_part if query_part is None else f"{path_part}?{query_part}"
    return applied


def _apply_header_substitutions(
    flow: http.HTTPFlow,
    substitutions: list[dict[str, Any]],
) -> list[str]:
    applied: list[str] = []
    replacements = _substitution_replacements(substitutions, "header")
    if not replacements:
        return applied
    for name, header_value in list(flow.request.headers.items()):
        if name.lower() in HEADER_SUBSTITUTION_DENYLIST:
            continue
        updated, header_applied = _replace_literals_once(str(header_value), replacements)
        if header_applied:
            flow.request.headers[name] = updated
            applied.extend("header" for _ in header_applied)
    return applied


def _apply_body_substitutions(
    flow: http.HTTPFlow,
    substitutions: list[dict[str, Any]],
) -> list[str]:
    body = _request_body_bytes(flow)
    if body is None:
        return []

    content_encoding = flow.request.headers.get("content-encoding", "").strip().lower()
    if content_encoding and content_encoding != "identity":
        return []

    content_type = flow.request.headers.get("content-type", "")
    if content_type.split(";", 1)[0].strip().lower().startswith("multipart/"):
        return []

    try:
        text = body.decode("utf-8")
    except UnicodeDecodeError:
        return []

    applied: list[str] = []
    replacements = _substitution_replacements(substitutions, "body", content_type)
    text, body_applied = _replace_literals_once(text, replacements)
    applied.extend("body" for _ in body_applied)

    if applied:
        _set_request_body_bytes(flow, text.encode("utf-8"))
    return applied


def _apply_requestheaders_substitutions(flow: http.HTTPFlow, binding: dict[str, Any]) -> list[str]:
    """Path/query/header substitutions (body not read yet at this stage)."""
    substitutions = binding.get("substitutions") or []
    if not substitutions:
        return []

    applied = []
    applied.extend(_apply_path_query_substitutions(flow, substitutions))
    if _flow_rejected(flow):
        return applied
    applied.extend(_apply_header_substitutions(flow, substitutions))
    return applied


def _has_substitutions_on(binding: dict[str, Any], surfaces: set[str]) -> bool:
    return any(
        surface in surfaces
        for substitution in (binding.get("substitutions") or [])
        for surface in (substitution.get("in") or [])
    )


def _flow_client_ip(flow: http.HTTPFlow) -> str | None:
    try:
        client_conn = getattr(flow, "client_conn", None)
        if client_conn is None:
            return None
        return client_conn.peername[0]
    except Exception:  # noqa: BLE001 - defensive; missing peername means no dispatch key
        return None


def requestheaders(flow: http.HTTPFlow) -> None:
    """Credential proxy phase 1: binding match and request metadata rewrite.

    Header injection must happen here, before the upstream connection is
    made: with ``stream_large_bodies=1m`` the ``request`` hook fires only
    after a body above 1 MiB has been streamed upstream.
    """
    vault = _load_active_vault(_flow_client_ip(flow))
    if vault is None:
        return

    # Requests outside credential binding scope are ordinary egress traffic.
    # Leave them untouched, including paths whose encoding would be ambiguous
    # for credential injection, because no secret is at risk.
    binding = _select_binding(flow, vault)
    if not binding:
        return

    # Reject ambiguous paths only for requests that would receive credentials:
    # dot-segments or encoded separators could redirect credentials to a scope
    # the canonical path does not match. A single-layer ``%2f`` is tolerated
    # here (npm scoped packages send ``/@scope%2fname``); the next check rejects
    # it if it crosses a binding boundary.
    raw_path = flow.request.path or "/"
    if _path_is_ambiguous(raw_path, allow_single_encoded_slash=True):
        _reject_request(flow, b"request path contains ambiguous segments\n")
        ctx.log.warn(
            "credential proxy: rejected request with ambiguous path: "
            f"{flow.request.method} {_request_host(flow)}{_request_path(flow)}"
        )
        return

    # Reject a ``%2f`` only when decoding it changes the binding match, so
    # ``/@scope%2fname`` stays working while crafted paths like
    # ``/api/v8/projects/123%2f..%2f456/...`` are stopped.
    if _path_encoded_slash_changes_binding(flow, vault):
        _reject_request(flow, b"request path contains ambiguous segments\n")
        ctx.log.warn(
            "credential proxy: rejected request whose encoded slash crosses "
            "the credential binding boundary: "
            f"{flow.request.method} {_request_host(flow)}{_request_path(flow)}"
        )
        return

    flow.metadata[FLOW_BINDING_KEY] = binding
    # Persist the redactions of the matched revision: body substitutions run
    # later in the request hook, and reloading the vault there could return a
    # different revision (0.5s cache TTL), leaving substituted credentials
    # unredactable in response headers.
    flow.metadata[FLOW_VAULT_REDACTIONS_KEY] = list(vault.redactions)

    substituted_surfaces = _apply_requestheaders_substitutions(flow, binding)
    if _flow_rejected(flow):
        return

    injected_names: list[str] = []
    for header in binding.get("headers") or []:
        name = header.get("name")
        value = header.get("value")
        if not name or value is None:
            continue
        # mitmproxy Headers is case-insensitive; delete first to avoid duplicate
        # effective header names before setting the credentialed value.
        if name in flow.request.headers:
            del flow.request.headers[name]
        flow.request.headers[name] = value
        injected_names.append(name)

    if injected_names or substituted_surfaces:
        flow.metadata[FLOW_REDACTIONS_KEY] = list(flow.metadata[FLOW_VAULT_REDACTIONS_KEY])
        ctx.log.info(
            "credential proxy: applied binding="
            f"{binding.get('name')} revision={vault.revision} "
            f"host={_request_host(flow)} method={flow.request.method} "
            f"headers={','.join(injected_names)} "
            f"substitutions={','.join(sorted(set(substituted_surfaces)))}"
        )
    elif _has_substitutions_on(binding, {"path", "query", "header"}):
        ctx.log.info(
            "credential proxy: substitution miss binding="
            f"{binding.get('name')} revision={vault.revision} "
            f"host={_request_host(flow)} method={flow.request.method}"
        )


def request(flow: http.HTTPFlow) -> None:
    """Credential proxy phase 2: body substitutions.

    Needs the full body, so it cannot run in ``requestheaders``. Streamed
    requests are skipped: the body was already forwarded upstream and is no
    longer available or modifiable.
    """
    binding = flow.metadata.get(FLOW_BINDING_KEY)
    if binding is None:
        return
    if getattr(flow.request, "stream", False):
        return

    applied = _apply_body_substitutions(flow, binding.get("substitutions") or [])
    if applied:
        if FLOW_REDACTIONS_KEY not in flow.metadata:
            flow.metadata[FLOW_REDACTIONS_KEY] = list(
                flow.metadata.get(FLOW_VAULT_REDACTIONS_KEY, [])
            )
        ctx.log.info(
            "credential proxy: applied body substitutions binding="
            f"{binding.get('name')} host={_request_host(flow)} "
            f"method={flow.request.method}"
        )
    elif FLOW_REDACTIONS_KEY not in flow.metadata and _has_substitutions_on(
        binding, {"body"}
    ):
        ctx.log.info(
            "credential proxy: substitution miss binding="
            f"{binding.get('name')} host={_request_host(flow)} "
            f"method={flow.request.method}"
        )


def responseheaders(flow: http.HTTPFlow) -> None:
    if flow.response is None:
        return
    _redact_response_headers(flow)
    content_type = flow.response.headers.get("content-type", "").lower()
    transfer_encoding = flow.response.headers.get("transfer-encoding", "").lower()
    if "text/event-stream" in content_type or "chunked" in transfer_encoding:
        flow.response.stream = True


def _redact_response_headers(flow: http.HTTPFlow) -> None:
    redactions = flow.metadata.get(FLOW_REDACTIONS_KEY, [])
    if not redactions or flow.response is None:
        return
    for name, value in list(flow.response.headers.items()):
        redacted = _redact_text(value, redactions)
        if redacted != value:
            flow.response.headers[name] = redacted


def _redact_text(text: str, values: list[str]) -> str:
    out = text
    for value in sorted({value for value in values if value}, key=len, reverse=True):
        out = out.replace(value, "[REDACTED]")
    return out
