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
from urllib.parse import unquote

from mitmproxy import ctx, http
from mitmproxy.tls import ClientHelloData


CREDENTIAL_PROXY_SOCKET_ENV = "OPENSANDBOX_CREDENTIAL_PROXY_SOCKET"
DEFAULT_CREDENTIAL_PROXY_SOCKET = "/run/opensandbox/credential-proxy/active.sock"
ACTIVE_VAULT_PATH = "/credential-vault/_active"
VAULT_CACHE_TTL_SECONDS = 0.5
FLOW_REDACTIONS_KEY = "opensandbox_credential_redactions"


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


def _is_ip_address(host: str) -> bool:
    """Return True if *host* looks like an IP address literal (v4 or v6)."""
    if not host:
        return False
    # IPv6 – bare or bracketed
    bare = host.strip("[]")
    if ":" in bare:
        return True
    # IPv4
    parts = bare.split(".")
    return len(parts) == 4 and all(p.isdigit() for p in parts)


def _flow_sni(flow: http.HTTPFlow) -> str:
    """Extract the TLS SNI hostname from the client connection, if available."""
    client_conn = getattr(flow, "client_conn", None)
    if client_conn is None:
        return ""
    sni = getattr(client_conn, "sni", None) or ""
    return sni.rstrip(".").lower()


def _credential_destination_mismatch(flow: http.HTTPFlow) -> bool:
    """Return True when the Host header diverges from the actual upstream.

    A mismatch indicates a potential credential-destination-confusion attack:
    the sandbox sets the Host header to a protected binding domain while the
    TCP connection targets a different (possibly attacker-controlled) host.
    """
    actual = (flow.request.host or "").rstrip(".").lower()
    presented = (flow.request.pretty_host or "").rstrip(".").lower()

    if not actual or not presented:
        return False

    # Non-IP destination: trust the FQDN match.
    if not _is_ip_address(actual):
        return actual != presented

    # Actual destination is an IP (transparent mode).
    sni = _flow_sni(flow)
    if sni:
        # SNI is certificate-verified – require it to match Host.
        return sni != presented

    # No SNI: HTTPS is unverifiable; HTTP is an accepted residual risk
    # mitigated by egress network policy and DNS enforcement.
    # FIXME: HTTP + IP destination + no SNI still trusts the spoofable Host
    # header via _request_host()'s pretty_host fallback, keeping the
    # destination-confusion path open for http bindings. Proper fix is to
    # reject `http` scheme bindings at the Go validation layer
    # (components/egress/pkg/credentialvault/vault.go); blocking it here would
    # break the transparent-HTTP credential-vault E2E
    # (tests/go/credential_vault_e2e_test.go), which relies on this fallback.
    return (flow.request.scheme or "").lower() == "https"


def _request_host(flow: http.HTTPFlow) -> str:
    """Return the hostname to match against credential-vault bindings.

    Prefers the actual upstream authority over the client-controlled Host
    header to prevent credential-destination-confusion attacks.

    Resolution order:
      1. ``flow.request.host`` when it is already an FQDN (non-transparent
         mode or upstream-resolved).
      2. TLS SNI from the client connection – mitmproxy validates the upstream
         certificate against this value so it cannot diverge from the real
         destination (``ssl_insecure`` is rejected by vault readiness checks).
      3. ``flow.request.pretty_host`` (Host header) as a last resort for
         transparent HTTP where the destination is an IP and no SNI exists.
    """
    actual = (flow.request.host or "").rstrip(".").lower()

    # Already an FQDN – use directly.
    if actual and not _is_ip_address(actual):
        return actual

    # Transparent HTTPS – SNI is certificate-verified.
    sni = _flow_sni(flow)
    if sni:
        return sni

    # Transparent HTTP fallback – weakest signal.
    presented = (flow.request.pretty_host or "").rstrip(".").lower()
    return presented or actual or ""


def _request_port(flow: http.HTTPFlow) -> int:
    if flow.request.port:
        return int(flow.request.port)
    return 443 if flow.request.scheme == "https" else 80


def _request_path(flow: http.HTTPFlow) -> str:
    path = flow.request.path or "/"
    return path.split("?", 1)[0] or "/"


_DOT_SEGMENT_RE = re.compile(r"/\.\.(/|$)")


def _path_is_ambiguous(raw_path: str) -> bool:
    """Return True if the raw request path contains dot-segment traversal sequences.

    Legitimate HTTP clients resolve dot segments before sending. Raw ``..``
    or percent-encoded variants on the wire indicate an attempt to confuse
    path-based authorization checks (the canonical path seen by the upstream
    server would differ from the raw prefix matched here).
    """
    path = raw_path.split("?", 1)[0]

    # Only match ``..`` as a complete path segment (/../ or trailing /..).
    if _DOT_SEGMENT_RE.search(path):
        return True

    # Iteratively decode to catch nested encodings like %252e%252e.
    decoded = path
    for _ in range(10):
        next_decoded = unquote(decoded)
        if next_decoded == decoded:
            break
        decoded = next_decoded
    if _DOT_SEGMENT_RE.search(decoded):
        return True

    # Reject encoded separators (%2f, %5c) and raw backslashes.
    lower = path.lower()
    if "%2f" in lower or "%5c" in lower:
        return True
    if "\\" in path:
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


def request(flow: http.HTTPFlow) -> None:
    vault = _load_active_vault()
    if vault is None:
        return

    # Guard: reject credential injection when the Host header diverges from
    # the actual upstream destination (credential-destination-confusion).
    if _credential_destination_mismatch(flow):
        presented = (flow.request.pretty_host or "").rstrip(".").lower()
        actual = (flow.request.host or "").rstrip(".").lower()
        ctx.log.warn(
            f"credential proxy: Host header {presented!r} does not match "
            f"upstream destination {actual!r}; skipping credential injection"
        )
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

    if injected_names:
        flow.metadata[FLOW_REDACTIONS_KEY] = list(vault.redactions)
        ctx.log.info(
            "credential proxy: injected binding="
            f"{binding.get('name')} revision={vault.revision} "
            f"host={_request_host(flow)} method={flow.request.method} "
            f"headers={','.join(injected_names)}"
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
    for value in values:
        if value:
            out = out.replace(value, "[REDACTED]")
    return out
