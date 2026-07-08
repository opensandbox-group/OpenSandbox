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
#      Credential Vault revision. The active revision is stored in the Go
#      sidecar process and read over an egress-container-private Unix socket.
#      Credential values are not logged, and response header values containing
#      credentials are redacted. Response bodies are not rewritten by default.
#   3. Implements SNI-aware ignore_hosts for transparent mode. mitmproxy's
#      built-in ignore_hosts check in transparent mode matches against the
#      destination IP first; the SNI hostname is only available inside the TLS
#      ClientHello, which arrives after the initial check. This addon re-checks
#      the same ignore_hosts patterns against the SNI hostname at the
#      tls_clienthello layer and sets ignore_connection=True when a match is
#      found, ensuring domain-based TLS pass-through works reliably.
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
    """
    sni = data.client_hello.sni
    if not sni:
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


def _load_active_vault() -> ActiveVault | None:
    global _vault_cache, _vault_cache_loaded_at
    now = time.monotonic()
    if _vault_cache is not None and now - _vault_cache_loaded_at < VAULT_CACHE_TTL_SECONDS:
        return _vault_cache

    socket_path = (
        os.environ.get(CREDENTIAL_PROXY_SOCKET_ENV, "").strip()
        or DEFAULT_CREDENTIAL_PROXY_SOCKET
    )
    connection = UnixSocketHTTPConnection(socket_path, timeout=0.25)
    try:
        connection.request("GET", ACTIVE_VAULT_PATH)
        response = connection.getresponse()
        body = response.read()
        if response.status == 404:
            _vault_cache = None
            _vault_cache_loaded_at = now
            return None
        if response.status != 200:
            ctx.log.warn(
                f"credential proxy: active vault lookup failed with HTTP {response.status}"
            )
            _vault_cache = None
            _vault_cache_loaded_at = now
            return None
        payload = json.loads(body.decode("utf-8"))
    except Exception as exc:  # noqa: BLE001 - mitm addon must not crash traffic handling
        ctx.log.warn(f"credential proxy: active vault lookup failed: {exc}")
        _vault_cache = None
        _vault_cache_loaded_at = now
        return None
    finally:
        connection.close()

    bindings = payload.get("bindings") or []
    redactions = [v for v in (payload.get("redactions") or []) if isinstance(v, str) and v]
    _vault_cache = ActiveVault(
        revision=int(payload.get("revision") or 0),
        bindings=bindings,
        redactions=redactions,
    )
    _vault_cache_loaded_at = now
    return _vault_cache


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


def _path_is_ambiguous(raw_path: str) -> bool:
    """Return True if the raw request path contains ambiguous segments.

    Legitimate HTTP clients resolve dot segments before sending. Raw ``..``
    or percent-encoded separators on the wire indicate an attempt to confuse
    path-based authorization checks because the canonical path seen by the
    upstream server may differ from the raw prefix matched here.
    """
    path = raw_path.split("?", 1)[0]

    # Only match ``..`` as a complete path segment (/../ or trailing /..).
    if _DOT_SEGMENT_RE.search(path):
        return True

    # Iteratively decode to catch nested encodings like %252e%252e or %252f.
    decoded = path
    for _ in range(10):
        lower = decoded.lower()
        if "%2f" in lower or "%5c" in lower:
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
        flow.response = http.Response.make(
            403,
            b"credential binding ambiguous\n",
            {"content-type": "text/plain"},
        )
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
            flow.response = http.Response.make(
                403,
                b"request path contains ambiguous substituted segments\n",
                {"content-type": "text/plain"},
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


def _apply_substitutions(flow: http.HTTPFlow, binding: dict[str, Any]) -> list[str]:
    substitutions = binding.get("substitutions") or []
    if not substitutions:
        return []

    applied = []
    applied.extend(_apply_path_query_substitutions(flow, substitutions))
    if flow.response is not None and getattr(flow.response, "status_code", None) == 403:
        return applied
    applied.extend(_apply_header_substitutions(flow, substitutions))
    applied.extend(_apply_body_substitutions(flow, substitutions))
    return applied


def request(flow: http.HTTPFlow) -> None:
    vault = _load_active_vault()
    if vault is None:
        return

    # Reject requests with ambiguous path segments before credential injection.
    # Raw dot-segments or encoded variants on the wire are not produced by
    # legitimate HTTP clients and can trick prefix-based path matching into
    # injecting credentials for a scope the canonical path does not belong to.
    raw_path = flow.request.path or "/"
    if _path_is_ambiguous(raw_path):
        flow.response = http.Response.make(
            403,
            b"request path contains ambiguous segments\n",
            {"content-type": "text/plain"},
        )
        ctx.log.warn(
            "credential proxy: rejected request with ambiguous path: "
            f"{flow.request.method} {_request_host(flow)}{_request_path(flow)}"
        )
        return

    binding = _select_binding(flow, vault)
    if not binding:
        return

    substituted_surfaces = _apply_substitutions(flow, binding)
    if flow.response is not None and getattr(flow.response, "status_code", None) == 403:
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
        flow.metadata[FLOW_REDACTIONS_KEY] = list(vault.redactions)
        ctx.log.info(
            "credential proxy: applied binding="
            f"{binding.get('name')} revision={vault.revision} "
            f"host={_request_host(flow)} method={flow.request.method} "
            f"headers={','.join(injected_names)} "
            f"substitutions={','.join(sorted(set(substituted_surfaces)))}"
        )
    elif binding.get("substitutions"):
        ctx.log.info(
            "credential proxy: substitution miss binding="
            f"{binding.get('name')} revision={vault.revision} "
            f"host={_request_host(flow)} method={flow.request.method}"
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
