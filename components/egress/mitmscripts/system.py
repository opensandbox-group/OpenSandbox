# Copyright 2026 The OpenSandbox Authors
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
#      socket. Every new flow conditionally checks the cached snapshot tag;
#      only a changed tag transfers a full snapshot. Lookup or protocol failures
#      fail closed for all intercepted requests, including hosts outside any
#      binding scope. Credential values are never logged; response header values
#      containing them are redacted. Response bodies are not rewritten.
#      Exchange bindings (OSEP-0025) carry only a marker in the snapshot:
#      the async `requestheaders` hook resolves them per request on the
#      active socket `_resolve` endpoint and fails closed (407/kill) on any
#      failure; static bindings stay inline and never suspend the hook.
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
#   5. Owns the authenticated revision receiver only when the Go launcher hands
#      off a complete per-process internal session. The default remains disabled.
#      When that session also carries the live admission bundle, TLS decisions
#      and request admission run against the installed immutable decision
#      snapshot (credential_bound.py): unbound HTTPS passes through opaquely,
#      bound hosts are decrypted only with a registry admission, and requests
#      are authorized per revision with bounded drain on removal.
#
# User-defined addons can be loaded alongside this script via
# OPENSANDBOX_EGRESS_MITMPROXY_SCRIPT (comma-separated for multiple scripts).
from __future__ import annotations

import asyncio
import http.client as http_client
import ipaddress
import json
import os
import re
import socket
import sys
import time
from contextlib import suppress
from typing import TYPE_CHECKING, Any, Awaitable, NoReturn
from urllib.parse import quote, quote_plus, unquote
from uuid import UUID

from mitmproxy import ctx, http
from mitmproxy.tls import ClientHelloData

if TYPE_CHECKING:
    from mitmproxy.tls import TlsData
    from revision_receiver import Revision

CREDENTIAL_PROXY_SOCKET_ENV = "OPENSANDBOX_CREDENTIAL_PROXY_SOCKET"
DEFAULT_CREDENTIAL_PROXY_SOCKET = "/run/opensandbox/credential-proxy/active.sock"
ACTIVE_VAULT_PATH = "/credential-vault/_active"
ACTIVE_VAULT_SUBJECT_HEADER = "X-OpenSandbox-Subject"
CREDENTIAL_RESOLVE_PATH = "/credential-vault/_resolve"
# Mirrors the Go resolver timeout env (seconds, 1..120, default 2); the fixed
# margin lets the egress deliver its own timeout classification first.
CREDENTIAL_RESOLVER_TIMEOUT_ENV = "OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_TIMEOUT"
CREDENTIAL_RESOLVER_MAX_CONCURRENCY_ENV = (
    "OPENSANDBOX_EGRESS_CREDENTIAL_RESOLVER_MAX_CONCURRENCY"
)
CREDENTIAL_RESOLVE_TIMEOUT_MARGIN_SECONDS = 0.25
DEFAULT_CREDENTIAL_RESOLVER_TIMEOUT_SECONDS = 2
CREDENTIAL_RESOLVER_TIMEOUT_BOUNDS = (1, 120)
DEFAULT_CREDENTIAL_RESOLVER_CONCURRENCY = 32
CREDENTIAL_RESOLVER_CONCURRENCY_BOUNDS = (1, 1024)
MAX_RESOLVE_RESPONSE_BYTES = 256 * 1024
# Forwarded request headers for exchange resolves: structural/hop-by-hop
# fields are excluded, and the caps mirror the Go `_resolve` endpoint — the
# addon fails closed on all of them first, payload size included.
MAX_RESOLVE_REQUEST_BYTES = 64 * 1024
MAX_RESOLVE_REQUEST_HEADERS = 64
MAX_RESOLVE_REQUEST_HEADER_VALUE_BYTES = 8 * 1024
RESOLVE_REQUEST_HEADER_DENYLIST = {
    "host",
    "content-length",
    "transfer-encoding",
    "connection",
    "keep-alive",
    "upgrade",
    "te",
    "trailer",
    "expect",
    "proxy-connection",
    "proxy-authorization",
    "proxy-authenticate",
}
FLOW_REDACTIONS_KEY = "opensandbox_credential_redactions"
FLOW_BINDING_KEY = "opensandbox_credential_binding"
FLOW_BINDINGS_KEY = "opensandbox_credential_bindings"
FLOW_VAULT_REDACTIONS_KEY = "opensandbox_credential_vault_redactions"
FLOW_VAULT_SUPPRESS_KEY = "opensandbox_credential_vault_suppress"
FLOW_REJECTION_KEY = "opensandbox_credential_rejected"
FLOW_SUBJECT_KEY = "opensandbox_credential_subject"
FLOW_SUBJECT_REQUEST_KEY = "opensandbox_subject_request"
FLOW_REQUEST_HANDLE_KEY = "opensandbox_credential_request_handle"
FLOW_H2_AUTHORITY_KEY = "opensandbox_credential_h2_authority"
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
ACTIVE_VAULT_HEADER_RESERVED_NAMES = {
    "host",
    "content-length",
    "content-type",
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
_ACTIVE_VAULT_HEADER_NAME_RE = re.compile(r"^[A-Za-z0-9!#$%&'*+\-.^_`|~]+$")
_ACTIVE_VAULT_HOST_LABEL_RE = re.compile(r"^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$")
_PERCENT_ESCAPE_RE = re.compile(r"%([0-9A-Fa-f]{2})")
# Strict snapshot key sets: unknown fields reject the whole snapshot
# (OSEP-0025 version-skew gate) instead of silently degrading bindings.
_ACTIVE_VAULT_TOP_LEVEL_FIELDS = {"revision", "bindings", "redactions"}
_ACTIVE_VAULT_BINDING_FIELDS = {
    "name",
    "match",
    "headers",
    "substitutions",
    "exchange",
}
# `ports` is deprecated upstream but still rendered by the Go snapshot for
# legacy vaults; allowed and ignored here.
_ACTIVE_VAULT_MATCH_FIELDS = {"schemes", "ports", "hosts", "methods", "paths"}
_ACTIVE_VAULT_HEADER_FIELDS = {"name", "value"}
_ACTIVE_VAULT_SUBSTITUTION_FIELDS = {"placeholder", "value", "in"}
_RESOLVE_RESPONSE_FIELDS = {"revision", "binding", "headers", "redactions", "suppressHeaders"}
_RESOLVE_GROUP_RESPONSE_FIELDS = {"decisions"}
_RESOLVE_HEADER_FIELDS = {"name", "value"}
_ENV_INT_RE = re.compile(r"[+-]?[0-9]+")
_REVISION_RUNTIME_ERROR = "credential proxy: invalid revision runtime configuration"
_REVISION_ENV = {
    "socket": "OPENSANDBOX_EGRESS_REVISION_IPC_SOCKET",
    "token": "OPENSANDBOX_EGRESS_REVISION_IPC_TOKEN",
    "control": "OPENSANDBOX_EGRESS_REVISION_CONTROL_GENERATION",
    "subject": "OPENSANDBOX_EGRESS_REVISION_SUBJECT_GENERATION",
    "limit": "OPENSANDBOX_EGRESS_REVISION_MAX_SNAPSHOT_BYTES",
}


class ActiveVault:
    def __init__(
        self,
        revision: int,
        bindings: list[dict[str, Any]],
        redactions: list[str],
        etag: str = "",
        subject: str = "",
    ) -> None:
        self.revision = revision
        self.bindings = bindings
        self.redactions = redactions
        self.etag = etag
        self.subject = subject


class ActiveVaultLookupError(Exception):
    """The active vault could not be validated for this flow."""


_vault_cache: ActiveVault | None = None
_revision_receiver: Any | None = None
_revision_server: Any | None = None
_live_runtime: Any | None = None
# Read-only binding views keyed by the complete, already-verified Revision.
# Keep only one entry, until the next cache miss or done(); this does not promise
# immediate wiping of retired credentials or mutate views held by older flows.
_live_vault_cache: tuple[Revision, ActiveVault | None] | None = None

# Operator-only diagnostics; no public interception mode is enabled here.
_tls_shadow_enabled = os.environ.get(
    "OPENSANDBOX_EGRESS_MITMPROXY_SHADOW", ""
).strip().lower() in {"1", "true", "on"}

# Fast Sandbox profile: one shared mitmdump serving N sandboxes; the active vault is
# selected by the client's source IP (preserved by the interception DNAT), so
# the immutable snapshot cache is keyed per client IP. Every flow performs a
# conditional snapshot-tag check; the full secret-bearing snapshot is transferred
# only when its opaque tag changes. The sidecar profile keeps one shared cache.
# The per-IP cache is bounded because spoofed source IPs could otherwise grow
# it without limit.
_fast_sandbox_mode_enabled = False
_vault_cache_by_ip: dict[str, ActiveVault] = {}
_VAULT_CACHE_MAX_IPS = 4096


def _set_fast_sandbox_mode(enabled: bool) -> None:
    global _fast_sandbox_mode_enabled
    _fast_sandbox_mode_enabled = enabled


def _set_fast_sandbox_mode_from_env() -> None:
    _set_fast_sandbox_mode(
        os.environ.get("OPENSANDBOX_EGRESS_PROFILE", "").strip().lower() == "fast-sandbox"
    )


_set_fast_sandbox_mode_from_env()


def _fatal_revision_runtime() -> NoReturn:
    # mitmproxy 11 loads -s scripts in a reload watcher. Generic exceptions are
    # swallowed and OptionsError only stops that watcher, so SystemExit is the
    # process-level fence that prevents a listener without the system addon.
    raise SystemExit(_REVISION_RUNTIME_ERROR) from None


def _revision_configuration() -> tuple[str, str, str, str, int] | None:
    present = {key for key, name in _REVISION_ENV.items() if name in os.environ}
    if not present:
        return None
    if present != set(_REVISION_ENV):
        _fatal_revision_runtime()
    values = {key: os.environ[name] for key, name in _REVISION_ENV.items()}
    limit_text = values["limit"]
    if (
        any(not value for value in values.values())
        or not limit_text.isascii()
        or not limit_text.isdecimal()
    ):
        _fatal_revision_runtime()
    try:
        limit = int(limit_text)
    except ValueError:
        _fatal_revision_runtime()
    if limit <= 0 or limit >= 2**63 or str(limit) != limit_text:
        _fatal_revision_runtime()
    return (
        values["socket"],
        values["token"],
        values["control"],
        values["subject"],
        limit,
    )


def load(_loader: Any) -> None:
    """Start the revision receiver the launcher selected for this process."""
    global _revision_receiver, _revision_server, _live_runtime
    if _revision_receiver is not None or _revision_server is not None or _live_runtime is not None:
        _fatal_revision_runtime()
    configuration = _revision_configuration()
    if configuration is None:
        return
    socket_path, token, control, subject, limit = configuration
    # The live credential-bound mode never runs with insecure upstream
    # verification: check the effective option before the receiver starts.
    # The installation-only receiver keeps the legacy escape hatch.
    if _live_admission_requested():
        _validate_live_options()
    receiver = server = runtime = None
    try:
        from revision_ipc import Server

        if _live_admission_requested():
            import credential_bound

            runtime, server = credential_bound.configure(
                socket_path, token, control, subject, limit
            )
            receiver = runtime.registry
        else:
            from revision_publication import InstallationReceiver

            receiver = InstallationReceiver(
                control,
                subject,
                max_snapshot_bytes=limit,
            )
            server = Server(
                receiver,
                socket_path,
                token,
                max_snapshot_bytes=limit,
                request_timeout=1,
            )
            server.start()
    except Exception:  # noqa: BLE001 - configuration may contain credentials
        if runtime is not None:
            with suppress(Exception):
                runtime.close()
        if server is not None:
            with suppress(Exception):
                server.close()
        elif receiver is not None:
            with suppress(Exception):
                receiver.close()
        _fatal_revision_runtime()
    _revision_receiver = receiver
    _revision_server = server
    _live_runtime = runtime


def _live_admission_requested() -> bool:
    """True when the launcher supplied any live admission budget variable."""
    try:
        import credential_bound

        return credential_bound.live_admission_requested()
    except Exception:  # noqa: BLE001 - only the launcher bundle matters here
        return False


def configure(updated: set) -> None:
    """Fence insecure TLS and native pre-TLS bypasses in the live runtime."""
    if not {"ssl_insecure", "ignore_hosts", "allow_hosts"}.intersection(updated):
        return
    if (_live_runtime is None and not _live_admission_requested()
            and _subject_admission_runtime() is None):
        return
    _validate_live_options()


def _validate_live_options() -> None:
    from credential_bound import validate_live_options

    validate_live_options(ctx.options)


def done() -> None:
    """Fence the receiver and remove only its owned socket during addon exit."""
    global _revision_receiver, _revision_server, _live_runtime, _live_vault_cache
    server = _revision_server
    runtime = _live_runtime
    _revision_receiver = None
    _revision_server = None
    _live_runtime = None
    _live_vault_cache = None
    if runtime is not None:
        try:
            runtime.close()
        except Exception:  # noqa: BLE001 - never expose session-bearing details
            ctx.log.warn("credential proxy: revision runtime cleanup failed")
    if server is not None:
        try:
            server.close()
        except Exception:  # noqa: BLE001 - never expose session-bearing details
            ctx.log.warn("credential proxy: revision runtime cleanup failed")


class UnixSocketHTTPConnection(http_client.HTTPConnection):
    def __init__(self, socket_path: str, timeout: float) -> None:
        super().__init__("credential-proxy", timeout=timeout)
        self.socket_path = socket_path

    def connect(self) -> None:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        try:
            sock.settimeout(self.timeout)
            sock.connect(self.socket_path)
        except Exception:
            # A failed connect must not leak the freshly created socket.
            sock.close()
            raise
        self.sock = sock


# OpenSSL X509_V_ERR_* values; never classify a raw TLS error string.
_UPSTREAM_VERIFY_REASONS = {
    10: "expired",
    18: "self_signed",
    19: "self_signed",
    20: "unknown_ca",
    21: "unknown_ca",
    62: "hostname_mismatch",
    64: "hostname_mismatch",
}


def tls_failed_server(data: TlsData) -> None:
    """Report upstream TLS failure without changing forwarding or trust."""
    from OpenSSL import SSL

    reason = "unknown"
    conn = data.ssl_conn
    if isinstance(conn, SSL.Connection):
        with suppress(AttributeError, TypeError, ValueError, SSL.Error):
            if conn.get_context().get_verify_mode() & SSL.VERIFY_PEER:
                # mitmproxy's pinned TLS layer uses this same OpenSSL result.
                result = SSL._lib.SSL_get_verify_result(conn._ssl)
                reason = _UPSTREAM_VERIFY_REASONS.get(result, "unknown")
    with suppress(AttributeError, TypeError, ValueError):
        record = {
            "level": "warn",
            "component": "egress",
            "event": "upstream_tls_failure",
            "reason": reason,
            "connection_id": str(UUID(str(data.conn.id))),
        }
        ctx.log.warn("credential proxy: upstream-tls-failure " + json.dumps(record))


def tls_clienthello(data: ClientHelloData) -> None:
    """Decide TLS interception for this ClientHello.

    Live credential-bound mode (OSEP-0023 experimental sidecar): classify
    against the installed immutable decision snapshot. Unbound SNI, ECH-hidden
    or no-SNI traffic passes through opaquely; a bound host is decrypted only
    with a registry admission; any deny outcome terminates the connection
    instead of decrypting or tunneling an unknown state.

    Legacy mode: re-check ignore_hosts patterns against the SNI hostname.

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
    subject = _subject_admission_runtime()
    if subject is not None:
        try:
            _tls_clienthello_live(subject, data, subject=True)
        except Exception:  # noqa: BLE001 - hook exceptions must not decrypt by default
            _record_tls_admission("deny", "invalid_input")
            subject.terminate_handshake(data.context.client)
        return
    live = _live_runtime
    if live is not None:
        _tls_clienthello_live(live, data)
        return

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


def next_layer(data: Any) -> None:
    """Install a fixed-version H2 instance before the selected HttpLayer starts."""
    live = _live_runtime
    client = data.context.client
    if live is None or getattr(client, "alpn", None) != b"h2":
        return
    token = live.token_for(client)
    if token is None:
        return
    try:
        from credential_bound_h2 import BoundH2Server, dependency_versions_supported
        from mitmproxy.proxy.layers.http import HttpLayer

        if (
            not dependency_versions_supported()
            or getattr(data.context.options, "connection_strategy", None) != "lazy"
            or getattr(data.context.server, "via", None) is not None
        ):
            # A chained proxy has a different physical-transport owner. It is
            # outside this direct/lazy proof; reject before request admission.
            raise ValueError("unsupported H2 transport")
        if data.layer is None:
            selector = ctx.master.addons.get("nextlayer")
            if selector is None:
                raise ValueError("missing layer selector")
            selector.next_layer(data)
        selected = data.layer
        # TLS and other outer layers still get their normal selector behavior.
        if not isinstance(selected, HttpLayer):
            return
        existing = live.h2_for(client)
        if existing is not None:
            if selected.connections.get(client) is existing:
                return
            raise ValueError("duplicate H2 owner")
        if client in selected.connections:
            raise ValueError("HTTP layer already started")
        adapter = BoundH2Server(selected.context.fork(), live, selected, token)
        selected.connections[client] = adapter
        if not live.attach_h2(client, adapter):
            raise ValueError("H2 attachment rejected")
    except Exception:  # noqa: BLE001 - only fixed diagnostics cross this boundary
        live._close_established(client)
        ctx.log.warn("credential proxy: H2 adapter unavailable; connection denied")


def server_connect(data: Any) -> None:
    live = _live_runtime
    if live is not None:
        live.server_connect(data)


def server_connect_error(data: Any) -> None:
    live = _live_runtime
    if live is not None:
        live.server_connect_error(data)


def server_connected(data: Any) -> None:
    live = _live_runtime
    if live is not None:
        live.server_connected(data)


def server_disconnected(data: Any) -> None:
    live = _live_runtime
    if live is not None:
        live.server_disconnected(data)


def _clienthello_ech_hidden(client_hello: Any) -> bool | None:
    """Inspect the actual ClientHello extension list for ECH (0xfe0d).

    mitmproxy 11 exposes ``ClientHello.extensions`` as ``list[(int, bytes)]``;
    GREASE ECH values are treated as ECH-bearing because the inner name stays
    hidden either way. Returns True when the extension is present, False when
    the extension list is valid and empty of it, and None when the extension
    data is absent or malformed — uninspectable input must deny, never be
    silently treated as ECH-free.
    """
    try:
        extensions = client_hello.extensions
    except Exception:  # noqa: BLE001 - never trust uninspectable hello data
        return None
    if not isinstance(extensions, (list, tuple)):
        return None
    for extension in extensions:
        if not (
            isinstance(extension, tuple)
            and len(extension) == 2
            and type(extension[0]) is int
            and isinstance(extension[1], (bytes, bytearray))
        ):
            return None
        if extension[0] == 0xFE0D:
            return True
    return False


# Closed admission outcomes only; never include SNI, credentials or raw errors.
_TLS_ADMISSION_REASONS = {
    "decrypt": ("binding_host",),
    "passthrough": ("ech", "no_sni", "static_ignore", "no_binding_host"),
    "deny": ("unknown_identity", "invalid_sni", "snapshot_missing",
             "generation_mismatch", "invalid_input", "non_canonical_port", "registry_exhausted",
             "admission_disabled", "ssl_insecure", "malformed_extensions"),
}


def _record_tls_admission(decision: str, reason: str) -> None:
    """Best-effort admission event, not handshake success or request count."""
    with suppress(Exception):
        if reason in _TLS_ADMISSION_REASONS.get(decision, ()):
            ctx.log.warn(f"credential proxy: tls-admission-v1 {decision} {reason}")


def _subject_admission_runtime() -> Any:
    # subject_runtime.load() guards its import with admission; subject_revision
    # imports lazily on enabled paths. The gate is runtime() is not None, not
    # module presence. Never import here: disabled deployments may omit it.
    module = sys.modules.get("subject_admission")
    return None if module is None else module.runtime()


def _terminate_denied_handshake(live: Any, data: ClientHelloData, *, subject: bool) -> None:
    """Close one denied handshake through the runtime that owns it.

    The Fast Sandbox subject runtime closes the exact client transport via
    the real mitmproxy connection handler; the sidecar path keeps calling
    the shared terminate_handshake with unchanged arguments and call count.
    """
    if subject:
        live.terminate_handshake(data.context.client)
        return
    import credential_bound

    credential_bound.terminate_handshake(data.context.client)


def _tls_clienthello_live(live: Any, data: ClientHelloData, *, subject: bool = False) -> None:
    # The live credential-bound mode must never run with insecure upstream
    # verification; deny the handshake outright instead of trusting an
    # unverified origin identity with a credential.
    if ctx.options.ssl_insecure:
        _record_tls_admission("deny", "ssl_insecure")
        with suppress(Exception):
            ctx.log.warn("credential proxy: tls decision deny reason=ssl_insecure")
        _terminate_denied_handshake(live, data, subject=subject)
        return
    # ECH hides the inner name: inspect the real extension list before any
    # outer-SNI matching so hidden names can never select a credential.
    ech_hidden = _clienthello_ech_hidden(data.client_hello)
    if ech_hidden is None:
        _record_tls_admission("deny", "malformed_extensions")
        with suppress(Exception):
            ctx.log.warn("credential proxy: tls decision deny reason=malformed_extensions")
        _terminate_denied_handshake(live, data, subject=subject)
        return
    sni = data.client_hello.sni
    from static_passthrough import destination_port

    port = destination_port(getattr(getattr(data.context, "server", None), "address", None))
    if subject:
        result = live.admit_tls(data.context.client, sni, ech_hidden=ech_hidden, destination_port=port)
    else:
        result = live.admit_tls(sni, ech_hidden=ech_hidden, destination_port=port)
    _record_tls_admission(result.action, result.reason)
    if result.action == "passthrough":
        data.ignore_connection = True
        return
    if result.action == "deny":
        # Fail closed: bootstrapping, generation mismatch, invalid or
        # unadmittable SNI, or an exhausted registry terminate the connection.
        # A deny never degrades into opaque tunneling or decrypt-all.
        with suppress(Exception):
            ctx.log.warn(f"credential proxy: tls decision deny reason={result.reason}")
        _terminate_denied_handshake(live, data, subject=subject)
        return
    if not subject:
        live.track(result.token, data.context.client)


def _load_active_vault(client_ip: str | None = None) -> ActiveVault | None:
    if _fast_sandbox_mode_enabled:
        return _load_active_vault_for_ip(client_ip)
    return _load_active_vault_shared()


def _load_active_vault_shared() -> ActiveVault | None:
    global _vault_cache
    try:
        vault = _fetch_active_vault(None, _vault_cache)
    except ActiveVaultLookupError:
        # Never retain a potentially revoked plaintext snapshot after the
        # proxy can no longer confirm its opaque tag.
        _vault_cache = None
        raise
    _vault_cache = vault
    return vault


def _load_active_vault_for_ip(client_ip: str | None) -> ActiveVault | None:
    if not client_ip:
        raise ActiveVaultLookupError("fast-sandbox vault lookup requires a client IP")
    if client_ip not in _vault_cache_by_ip and len(_vault_cache_by_ip) >= _VAULT_CACHE_MAX_IPS:
        _vault_cache_by_ip.clear()
    cached = _vault_cache_by_ip.get(client_ip)
    try:
        vault = _fetch_active_vault(client_ip, cached)
    except ActiveVaultLookupError:
        _vault_cache_by_ip.pop(client_ip, None)
        raise
    if vault is None:
        _vault_cache_by_ip.pop(client_ip, None)
    else:
        _vault_cache_by_ip[client_ip] = vault
    return vault


def _fetch_active_vault(
    client_ip: str | None = None,
    cached: ActiveVault | None = None,
) -> ActiveVault | None:
    socket_path = (
        os.environ.get(CREDENTIAL_PROXY_SOCKET_ENV, "").strip()
        or DEFAULT_CREDENTIAL_PROXY_SOCKET
    )
    path = ACTIVE_VAULT_PATH
    if client_ip:
        # fast-sandbox profile: one shared socket, dispatch inside — the handler
        # resolves clientIp -> subject -> that subject's vault snapshot
        path = f"{ACTIVE_VAULT_PATH}?clientIp={quote(client_ip)}"
    connection = UnixSocketHTTPConnection(socket_path, timeout=0.25)
    try:
        headers = {}
        if cached is not None:
            if not cached.etag:
                raise ActiveVaultLookupError("cached active vault has no ETag")
            headers["If-None-Match"] = cached.etag
        connection.request("GET", path, headers=headers)
        response = connection.getresponse()
        body = response.read()
        if response.status == 304:
            if cached is None:
                raise ActiveVaultLookupError(
                    "active vault returned 304 without a cached snapshot"
                )
            if response.getheader("ETag") != cached.etag:
                raise ActiveVaultLookupError("active vault 304 response has an invalid ETag")
            subject = _sanitize_log_token(response.getheader(ACTIVE_VAULT_SUBJECT_HEADER))
            if subject:
                cached.subject = subject
            return cached
        if response.status == 404:
            return None
        if response.status != 200:
            raise ActiveVaultLookupError(
                f"active vault lookup returned HTTP {response.status}"
            )
        payload = json.loads(body.decode("utf-8"))
        vault = _parse_active_vault(payload)
        etag = _validate_active_vault_etag(response.getheader("ETag"))
        if cached is not None and etag == cached.etag:
            raise ActiveVaultLookupError(
                "active vault tag did not advance after a conditional lookup"
            )
        vault.etag = etag
        vault.subject = _sanitize_log_token(response.getheader(ACTIVE_VAULT_SUBJECT_HEADER))
        return vault
    except ActiveVaultLookupError:
        raise
    except Exception as exc:
        raise ActiveVaultLookupError(f"active vault lookup failed: {exc}") from exc
    finally:
        with suppress(Exception):
            connection.close()


def _parse_active_vault(payload: Any) -> ActiveVault:
    if not isinstance(payload, dict):
        raise ActiveVaultLookupError("active vault payload must be an object")
    unexpected_fields = set(payload) - _ACTIVE_VAULT_TOP_LEVEL_FIELDS
    if unexpected_fields:
        raise ActiveVaultLookupError(
            "active vault payload contains unsupported fields: "
            + ",".join(sorted(unexpected_fields))
        )
    revision = payload.get("revision")
    if isinstance(revision, bool) or not isinstance(revision, int) or revision <= 0:
        raise ActiveVaultLookupError("active vault revision must be a positive integer")
    bindings = payload.get("bindings")
    if not isinstance(bindings, list) or any(not isinstance(item, dict) for item in bindings):
        raise ActiveVaultLookupError("active vault bindings must be a list of objects")
    redactions = payload.get("redactions", [])
    if not isinstance(redactions, list) or any(
        not isinstance(value, str) or not value for value in redactions
    ):
        raise ActiveVaultLookupError("active vault redactions must be non-empty strings")

    redaction_set = set(redactions)
    normalized_bindings: list[dict[str, Any]] = []
    for index, binding in enumerate(bindings):
        unexpected_fields = set(binding) - _ACTIVE_VAULT_BINDING_FIELDS
        if unexpected_fields:
            raise ActiveVaultLookupError(
                f"active vault binding {index} contains unsupported fields: "
                + ",".join(sorted(unexpected_fields))
            )
        name = binding.get("name")
        if not isinstance(name, str) or not name.strip():
            raise ActiveVaultLookupError(
                f"active vault binding {index} name must be a non-empty string"
            )
        if _LOG_CONTROL_CHAR_RE.search(name) is not None:
            raise ActiveVaultLookupError(
                f"active vault binding {index} name must not contain control characters"
            )

        exchange = _parse_active_vault_exchange(binding, index)

        match = binding.get("match")
        if not isinstance(match, dict):
            raise ActiveVaultLookupError(
                f"active vault binding {index} match must be an object"
            )
        unexpected_fields = set(match) - _ACTIVE_VAULT_MATCH_FIELDS
        if unexpected_fields:
            raise ActiveVaultLookupError(
                f"active vault binding {index} match contains unsupported fields: "
                + ",".join(sorted(unexpected_fields))
            )
        host_field = f"binding {index} match.hosts"
        hosts = [
            _normalize_active_vault_host(host, host_field)
            for host in _normalize_active_vault_strings(match.get("hosts"), host_field)
        ]
        schemes = _normalize_active_vault_strings(
            match.get("schemes"), f"binding {index} match.schemes", str.lower
        )
        if any(scheme not in {"http", "https"} for scheme in schemes):
            raise ActiveVaultLookupError(
                f"active vault binding {index} match.schemes contains an unsupported scheme"
            )
        methods = _normalize_active_vault_strings(
            match.get("methods"), f"binding {index} match.methods", str.upper
        )
        paths = _normalize_active_vault_strings(
            match.get("paths"), f"binding {index} match.paths"
        )
        if any(not path.startswith("/") for path in paths):
            raise ActiveVaultLookupError(
                f"active vault binding {index} match.paths must start with /"
            )

        raw_headers = binding.get("headers")
        if raw_headers is None:
            raw_headers = []
        if not isinstance(raw_headers, list) or any(
            not isinstance(header, dict) for header in raw_headers
        ):
            raise ActiveVaultLookupError(
                f"active vault binding {index} headers must be a list of objects"
            )
        normalized_headers: list[dict[str, str]] = []
        for header_index, header in enumerate(raw_headers):
            unexpected_fields = set(header) - _ACTIVE_VAULT_HEADER_FIELDS
            if unexpected_fields:
                raise ActiveVaultLookupError(
                    f"active vault binding {index} header {header_index} "
                    "contains unsupported fields: "
                    + ",".join(sorted(unexpected_fields))
                )
            header_name = header.get("name")
            header_value = header.get("value")
            if not isinstance(header_name, str) or not header_name.strip():
                raise ActiveVaultLookupError(
                    f"active vault binding {index} header {header_index} name "
                    "must be a non-empty string"
                )
            header_name = header_name.strip()
            if _ACTIVE_VAULT_HEADER_NAME_RE.fullmatch(header_name) is None:
                raise ActiveVaultLookupError(
                    f"active vault binding {index} header {header_index} name "
                    "is not a valid HTTP field name"
                )
            if header_name.lower() in ACTIVE_VAULT_HEADER_RESERVED_NAMES:
                raise ActiveVaultLookupError(
                    f"active vault binding {index} header {header_index} name "
                    "is reserved"
                )
            if not isinstance(header_value, str):
                raise ActiveVaultLookupError(
                    f"active vault binding {index} header {header_index} value "
                    "must be a string"
                )
            if header_value and header_value not in redaction_set:
                raise ActiveVaultLookupError(
                    f"active vault binding {index} header {header_index} value "
                    "is missing from redactions"
                )
            normalized_headers.append({"name": header_name, "value": header_value})

        raw_substitutions = binding.get("substitutions")
        if raw_substitutions is None:
            raw_substitutions = []
        if not isinstance(raw_substitutions, list) or any(
            not isinstance(substitution, dict) for substitution in raw_substitutions
        ):
            raise ActiveVaultLookupError(
                f"active vault binding {index} substitutions must be a list of objects"
            )
        normalized_substitutions: list[dict[str, Any]] = []
        for substitution_index, substitution in enumerate(raw_substitutions):
            unexpected_fields = set(substitution) - _ACTIVE_VAULT_SUBSTITUTION_FIELDS
            if unexpected_fields:
                raise ActiveVaultLookupError(
                    f"active vault binding {index} substitution {substitution_index} "
                    "contains unsupported fields: "
                    + ",".join(sorted(unexpected_fields))
                )
            placeholder = substitution.get("placeholder")
            value = substitution.get("value")
            if not isinstance(placeholder, str) or not placeholder.strip():
                raise ActiveVaultLookupError(
                    f"active vault binding {index} substitution {substitution_index} "
                    "placeholder must be a non-empty string"
                )
            if not isinstance(value, str):
                raise ActiveVaultLookupError(
                    f"active vault binding {index} substitution {substitution_index} "
                    "value must be a string"
                )
            surfaces = _normalize_active_vault_strings(
                substitution.get("in"),
                f"binding {index} substitution {substitution_index} in",
                str.lower,
            )
            if any(
                surface not in {"path", "query", "header", "body"}
                for surface in surfaces
            ):
                raise ActiveVaultLookupError(
                    f"active vault binding {index} substitution {substitution_index} "
                    "contains an unsupported surface"
                )
            missing_redactions = (
                _active_snapshot_substitution_redaction_variants(value) - redaction_set
            )
            if missing_redactions:
                raise ActiveVaultLookupError(
                    f"active vault binding {index} substitution {substitution_index} "
                    "value representations are missing from redactions"
                )
            if placeholder not in redaction_set:
                raise ActiveVaultLookupError(
                    f"active vault binding {index} substitution {substitution_index} "
                    "placeholder is missing from redactions"
                )
            normalized_substitutions.append(
                {"placeholder": placeholder, "value": value, "in": surfaces}
            )

        normalized_binding: dict[str, Any] = {
            "name": name.strip(),
            "match": {
                "schemes": schemes,
                "hosts": hosts,
                "methods": methods,
                "paths": paths,
            },
            "headers": normalized_headers,
            "substitutions": normalized_substitutions,
        }
        if exchange is not None:
            normalized_binding["exchange"] = exchange
        normalized_bindings.append(normalized_binding)
    return ActiveVault(
        revision=revision,
        bindings=normalized_bindings,
        redactions=list(redactions),
    )


def _parse_active_vault_exchange(binding: dict[str, Any], index: int) -> dict[str, str] | None:
    """Validate the optional exchange marker: exactly ``{"credential": name}``
    with explicitly empty ``headers``/``substitutions``; any present value
    (including ``null``) other than that shape fails the snapshot closed.
    """
    if "exchange" not in binding:
        return None
    raw_exchange = binding["exchange"]
    if not isinstance(raw_exchange, dict) or set(raw_exchange) != {"credential"}:
        raise ActiveVaultLookupError(
            f"active vault binding {index} exchange must contain only credential"
        )
    credential = raw_exchange["credential"]
    if not isinstance(credential, str) or not credential.strip():
        raise ActiveVaultLookupError(
            f"active vault binding {index} exchange credential must be a non-empty string"
        )
    for field in ("headers", "substitutions"):
        if binding.get(field) != []:
            raise ActiveVaultLookupError(
                f"active vault binding {index} with exchange must carry an "
                f"empty {field} list"
            )
    return {"credential": credential}


def _normalize_active_vault_strings(
    value: Any,
    field: str,
    normalize: Any | None = None,
) -> list[str]:
    if not isinstance(value, list) or not value:
        raise ActiveVaultLookupError(f"active vault {field} must be a non-empty list")
    normalized: list[str] = []
    for item in value:
        if not isinstance(item, str) or not item.strip():
            raise ActiveVaultLookupError(
                f"active vault {field} must contain non-empty strings"
            )
        item = item.strip()
        normalized.append(normalize(item) if normalize is not None else item)
    return normalized


def _normalize_active_vault_host(value: str, field: str) -> str:
    host = value.strip().lower()
    host = host.removesuffix(".")
    if not host or "://" in host or "/" in host:
        raise ActiveVaultLookupError(f"active vault {field} contains an invalid host")

    if host.startswith("*."):
        suffix = host[2:]
        if not suffix or "*" in suffix or _active_vault_host_is_ip(suffix):
            raise ActiveVaultLookupError(
                f"active vault {field} contains an invalid wildcard host"
            )
        if not _active_vault_host_is_fqdn(suffix):
            raise ActiveVaultLookupError(
                f"active vault {field} contains an invalid wildcard host"
            )
        return f"*.{suffix}"

    if "*" in host or _active_vault_host_is_ip(host):
        raise ActiveVaultLookupError(f"active vault {field} contains an invalid host")
    if not _active_vault_host_is_fqdn(host):
        raise ActiveVaultLookupError(f"active vault {field} contains an invalid host")
    return host


def _active_vault_host_is_ip(host: str) -> bool:
    try:
        ipaddress.ip_address(host)
    except ValueError:
        return False
    return True


def _active_vault_host_is_fqdn(host: str) -> bool:
    return (
        len(host) <= 253
        and "." in host
        and all(_ACTIVE_VAULT_HOST_LABEL_RE.fullmatch(label) for label in host.split("."))
    )


def _active_snapshot_substitution_redaction_variants(value: str) -> set[str]:
    url_encoded = quote(value, safe="")
    form_encoded = quote_plus(value, safe="")
    variants = {
        value,
        url_encoded,
        _lowercase_percent_escapes(url_encoded),
        form_encoded,
        _lowercase_percent_escapes(form_encoded),
        _go_json_encoded_string_content(value),
        json.dumps(value)[1:-1],
    }
    variants.discard("")
    return variants


def _lowercase_percent_escapes(value: str) -> str:
    return _PERCENT_ESCAPE_RE.sub(lambda match: f"%{match.group(1).lower()}", value)


def _go_json_encoded_string_content(value: str) -> str:
    encoded = json.dumps(value, ensure_ascii=False)[1:-1]
    return (
        encoded.replace("<", r"\u003c")
        .replace(">", r"\u003e")
        .replace("&", r"\u0026")
        .replace("\u2028", r"\u2028")
        .replace("\u2029", r"\u2029")
    )


def _validate_active_vault_etag(value: str | None) -> str:
    if value is None or re.fullmatch(r'"[A-Za-z0-9._~-]{1,128}"', value) is None:
        raise ActiveVaultLookupError("active vault response has an invalid ETag")
    return value


class CredentialResolveError(Exception):
    """Resolve failure with a log-safe reason: transport | invalid_response
    | deadline | http_<status>. Resolve content never reaches logs."""


def _env_int_range(name: str, default: int, bounds: tuple[int, int]) -> int:
    # Mirrors Go EnvIntRange: unparsable or out-of-bounds keeps the default.
    raw = os.environ.get(name, "").strip()
    if not raw or _ENV_INT_RE.fullmatch(raw) is None:
        return default
    value = int(raw)
    if not bounds[0] <= value <= bounds[1]:
        return default
    return value


def _resolve_budget_seconds() -> float:
    return (
        _env_int_range(
            CREDENTIAL_RESOLVER_TIMEOUT_ENV,
            DEFAULT_CREDENTIAL_RESOLVER_TIMEOUT_SECONDS,
            CREDENTIAL_RESOLVER_TIMEOUT_BOUNDS,
        )
        + CREDENTIAL_RESOLVE_TIMEOUT_MARGIN_SECONDS
    )


# Concurrency aligned with the Go endpoint's semaphore; keyed by loop
# because tests and script reloads swap event loops.
_resolve_limiter_state: tuple[asyncio.AbstractEventLoop, asyncio.Semaphore] | None = None


def _resolve_limiter() -> asyncio.Semaphore:
    global _resolve_limiter_state
    loop = asyncio.get_running_loop()
    state = _resolve_limiter_state
    if state is None or state[0] is not loop:
        state = (
            loop,
            asyncio.Semaphore(
                _env_int_range(
                    CREDENTIAL_RESOLVER_MAX_CONCURRENCY_ENV,
                    DEFAULT_CREDENTIAL_RESOLVER_CONCURRENCY,
                    CREDENTIAL_RESOLVER_CONCURRENCY_BOUNDS,
                )
            ),
        )
        _resolve_limiter_state = state
    return state[1]


def _parse_resolve_decision(document: Any, binding_name: str) -> dict[str, Any]:
    if not isinstance(document, dict) or set(document) != _RESOLVE_RESPONSE_FIELDS:
        raise CredentialResolveError("invalid_response")
    revision = document["revision"]
    if isinstance(revision, bool) or not isinstance(revision, int) or revision <= 0:
        raise CredentialResolveError("invalid_response")
    if document["binding"] != binding_name:
        raise CredentialResolveError("invalid_response")
    raw_headers = document["headers"]
    if not isinstance(raw_headers, list):
        raise CredentialResolveError("invalid_response")
    headers: list[dict[str, str]] = []
    for header in raw_headers:
        if not isinstance(header, dict) or set(header) != _RESOLVE_HEADER_FIELDS:
            raise CredentialResolveError("invalid_response")
        name = header["name"]
        value = header["value"]
        if (
            not isinstance(name, str)
            or not name.strip()
            or _ACTIVE_VAULT_HEADER_NAME_RE.fullmatch(name) is None
            or name.lower() in ACTIVE_VAULT_HEADER_RESERVED_NAMES
            or not isinstance(value, str)
            or not value
        ):
            raise CredentialResolveError("invalid_response")
        headers.append({"name": name, "value": value})
    redactions = document["redactions"]
    if not isinstance(redactions, list) or any(
        not isinstance(item, str) or not item for item in redactions
    ):
        raise CredentialResolveError("invalid_response")
    raw_suppress = document["suppressHeaders"]
    if not isinstance(raw_suppress, list):
        raise CredentialResolveError("invalid_response")
    suppress: set[str] = set()
    for name in raw_suppress:
        if (
            not isinstance(name, str)
            or not name.strip()
            or _ACTIVE_VAULT_HEADER_NAME_RE.fullmatch(name) is None
            or name.lower() in ACTIVE_VAULT_HEADER_RESERVED_NAMES
        ):
            raise CredentialResolveError("invalid_response")
        suppress.add(name.lower())
    return {
        "revision": revision,
        "headers": headers,
        "redactions": redactions,
        "suppressHeaders": suppress,
    }


def _parse_resolve_response(raw: bytes, binding_name: str) -> dict[str, Any]:
    """Strictly validate one 200 resolve body against the frozen contract."""
    if len(raw) > MAX_RESOLVE_RESPONSE_BYTES:
        raise CredentialResolveError("invalid_response")
    try:
        document = json.loads(raw.decode("utf-8"))
    except Exception:
        raise CredentialResolveError("invalid_response") from None
    return _parse_resolve_decision(document, binding_name)


def _parse_resolve_group_response(
    raw: bytes, binding_names: list[str], max_bytes: int
) -> list[dict[str, Any]]:
    """Strictly validate one 200 grouped resolve body."""
    if len(raw) > max_bytes:
        raise CredentialResolveError("invalid_response")
    try:
        document = json.loads(raw.decode("utf-8"))
    except Exception:
        raise CredentialResolveError("invalid_response") from None
    if not isinstance(document, dict) or set(document) != _RESOLVE_GROUP_RESPONSE_FIELDS:
        raise CredentialResolveError("invalid_response")
    raw_decisions = document["decisions"]
    if not isinstance(raw_decisions, list) or len(raw_decisions) != len(binding_names):
        raise CredentialResolveError("invalid_response")
    return [
        _parse_resolve_decision(item, name)
        for item, name in zip(raw_decisions, binding_names)
    ]


def _resolve_exchange_http(
    socket_path: str, timeout: float, path: str, payload: dict[str, Any]
) -> dict[str, Any]:
    """One resolve RPC (single or grouped); failures normalize to CredentialResolveError."""
    connection = UnixSocketHTTPConnection(socket_path, timeout=timeout)
    try:
        body = json.dumps(payload).encode("utf-8")
        connection.request(
            "POST", path, body=body, headers={"Content-Type": "application/json"}
        )
        response = connection.getresponse()
        # Bound the read itself; the size check must not follow an unbounded
        # buffer, multiplied by concurrent per-request resolves.
        is_group = "bindings" in payload
        max_bytes = MAX_RESOLVE_RESPONSE_BYTES
        if is_group:
            max_bytes = min(MAX_RESOLVE_RESPONSE_BYTES * max(1, len(payload["bindings"])), 4 << 20)
        raw = response.read(max_bytes + 1)
        if response.status != 200:
            raise CredentialResolveError(f"http_{response.status}")
        if is_group:
            return {
                "decisions": _parse_resolve_group_response(
                    raw, payload["bindings"], max_bytes
                )
            }
        return _parse_resolve_response(raw, payload["binding"])
    except CredentialResolveError:
        raise
    except Exception:
        raise CredentialResolveError("transport") from None
    finally:
        with suppress(Exception):
            connection.close()


def _request_host(flow: http.HTTPFlow) -> str:
    # H2 Request.authority/pretty_host decodes IDNA. Bindings use ASCII names;
    # use the already validated raw authority without rewriting the target.
    verified = flow.metadata.get(FLOW_H2_AUTHORITY_KEY)
    if verified is not None:
        return verified
    host = flow.request.pretty_host or flow.request.host or ""
    return host.rstrip(".").lower()


def _request_port(flow: http.HTTPFlow) -> int:
    if flow.request.port:
        return int(flow.request.port)
    return 443 if flow.request.scheme == "https" else 80


def _request_path(flow: http.HTTPFlow) -> str:
    path = flow.request.path or "/"
    return path.split("?", 1)[0] or "/"


def _resolve_request_headers(flow: http.HTTPFlow) -> dict[str, str]:
    """Select the outbound headers a resolver may bind its proof to.

    Everything except the structural denylist, lowercased. Field lines come
    from ``items(multi=True)``. A forwarded identity must appear exactly once:
    the payload carries a single value while mitmproxy forwards every line
    upstream, so a repeated (or case-variant) identity would let the resolver
    sign a value the upstream cannot reconstruct. Over-cap fails closed.

    The one exception is Cookie on HTTP/2 and HTTP/3, where clients may split
    it into one line per pair (RFC 9113 8.2.3). Those crumbs have a single
    joined form, every line "; "-joined in wire order with empty ones kept,
    which the destination rebuilds and mitmproxy sends to an HTTP/1 upstream,
    so the resolver gets that value.
    """
    http_version = (flow.request.http_version or "").upper()
    join_cookie_crumbs = http_version.startswith(("HTTP/2", "HTTP/3"))
    selected: dict[str, str] = {}
    lines: dict[str, int] = {}
    cookie_crumbs: list[str] = []
    for name, value in flow.request.headers.items(multi=True):
        lowered = name.lower()
        if lowered in RESOLVE_REQUEST_HEADER_DENYLIST:
            continue
        if lowered == "cookie" and join_cookie_crumbs:
            cookie_crumbs.append(value)
            continue
        lines[lowered] = lines.get(lowered, 0) + 1
        if not name or not value:
            continue
        selected[lowered] = _checked_resolve_header_value(value)
    if any(count > 1 for count in lines.values()):
        raise CredentialResolveError("resolve_ambiguous_header")
    cookie = "; ".join(cookie_crumbs)
    if cookie:
        selected["cookie"] = _checked_resolve_header_value(cookie)
    if len(selected) > MAX_RESOLVE_REQUEST_HEADERS:
        raise CredentialResolveError("resolve_too_many_headers")
    return selected


def _checked_resolve_header_value(value: str) -> str:
    """Return a forwarded header value, failing closed above the value cap."""
    if len(value.encode("utf-8")) > MAX_RESOLVE_REQUEST_HEADER_VALUE_BYTES:
        raise CredentialResolveError("resolve_header_too_long")
    return value


_DOT_SEGMENT_RE = re.compile(r"/\.\.(/|$)")


def _path_is_ambiguous(raw_path: str, *, allow_encoded_slash: bool = False) -> bool:
    """Return True if the raw request path could decode to a different path
    than the one used for binding match (dot-segments, backslashes, or a
    percent-decode that never converges). Legitimate clients resolve dot
    segments before sending, so ``..`` on the wire is an attempt to confuse
    path-based authorization.

    ``allow_encoded_slash`` tolerates ``%2f`` at any percent-decoding depth
    on the client-supplied path (legit for npm scoped package registry paths
    like ``/@scope%2fname`` and artifact-store coordinate paths like
    ``pkg%252F1.0``). Whether an encoded slash is safe is decided by the
    complementary :func:`_path_decoding_changes_binding` check, which rejects
    a path whose decoding crosses an authorization boundary. Backslashes and
    dot-segments are always rejected at every decoding depth.
    """
    path = raw_path.split("?", 1)[0]

    # Only match ``..`` as a complete path segment (/../ or trailing /..).
    if _DOT_SEGMENT_RE.search(path):
        return True

    # Iteratively decode to catch nested encodings like %252e%252e. Decoding
    # only turns ``%xx`` escapes into literal characters, so anything that
    # appears at an intermediate depth survives to the fixpoint; checking the
    # fixpoint covers every depth.
    decoded = path
    for _ in range(10):
        lower = decoded.lower()
        if not allow_encoded_slash and "%2f" in lower:
            return True
        if "%5c" in lower:
            return True
        if "\\" in decoded:
            return True
        next_decoded = unquote(decoded)
        if next_decoded == decoded:
            break
        decoded = next_decoded
    else:
        # No fixpoint within the iteration bound: absurdly nested escapes on
        # a client path mean we cannot establish a canonical view; fail closed.
        return True
    if _DOT_SEGMENT_RE.search(decoded):
        return True

    return False


def _path_decoding_changes_binding(
    flow: http.HTTPFlow, vault: ActiveVault
) -> bool:
    """Return True if percent-decoding the raw path to any depth would change
    which credential binding matches (i.e. decoding crosses an authorization
    boundary).

    This is the credential-injection invariant: the binding selected on the
    raw wire path must be identical for every decode depth, so the injection
    decision does not depend on how many times any downstream processor
    decodes the path. Legit encoded uses — npm scoped package registry paths
    like ``/@scope%2fname`` or artifact-store coordinate paths like
    ``pkg%252F1.0`` — decode to a path matching the same binding, so they
    pass; crafted paths like ``/api/v8/projects/123%2f..%2f456/variables``
    are rejected before credential injection.
    """
    raw_path = _request_path(flow)

    # Collect every intermediate view from the raw path down to the decode
    # fixpoint, so the invariant holds no matter how many times an encoded
    # slash is nested (%2f, %252f, %25252f, ...).
    views = [raw_path]
    decoded = raw_path
    for _ in range(10):
        next_decoded = unquote(decoded)
        if next_decoded == decoded:
            break
        decoded = next_decoded
        views.append(decoded)

    if len(views) == 1:
        return False

    # If any decoded form contains dot-segments, treat it as ambiguous.
    for view in views[1:]:
        if _DOT_SEGMENT_RE.search(view):
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

    raw_matches = _matches_with_path(raw_path)
    return any(_matches_with_path(view) != raw_matches for view in views[1:])


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
    HTTP/2 bodies without Content-Length). A local response cannot be served
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


def _reject_request(
    flow: http.HTTPFlow,
    body: bytes,
    status_code: int = 403,
) -> None:
    """Terminate a request before it is forwarded upstream.

    mitmproxy 11.0.2 refuses to serve a locally-set response while a request
    body is being streamed: ``start_request_stream`` raises
    ``NotImplementedError`` once ``flow.response`` is set, and streaming is
    enabled as soon as a body is known to exceed ``stream_large_bodies``
    (1 MiB) — either up front via Content-Length or mid-upload for chunked
    bodies. A local response is therefore only safe when the body size is
    fully known; otherwise the flow is killed, which closes the client
    connection without forwarding anything.
    """
    flow.metadata[FLOW_REJECTION_KEY] = True
    if _request_may_be_streamed(flow):
        if flow.killable:
            flow.kill()
        return
    flow.response = http.Response.make(status_code, body, {"content-type": "text/plain"})


def _flow_rejected(flow: http.HTTPFlow) -> bool:
    """True if the flow was terminated by :func:`_reject_request`."""
    return bool(flow.metadata.get(FLOW_REJECTION_KEY))


_LOG_TOKEN_RE = re.compile(r"^[A-Za-z0-9._:@/+=-]{1,160}$")
_LOG_CONTROL_CHAR_RE = re.compile(r"[\x00-\x1f\x7f-\x9f\u2028\u2029]")


def _escape_log_text(value: Any) -> str:
    # Escape control characters and Unicode line separators so untrusted or
    # writer-supplied fields cannot split a mitmdump terminal line into a forged
    # ``credential proxy:`` record forwarded by forwardMitmdumpOutput.
    return _LOG_CONTROL_CHAR_RE.sub(
        lambda match: match.group(0).encode("unicode_escape").decode("ascii"),
        str(value),
    )


def _emit_credential_log(level: str, message: str) -> None:
    getattr(ctx.log, level)(_escape_log_text(message))


def _sanitize_log_token(value: Any) -> str:
    if not isinstance(value, str):
        return ""
    token = value.strip()
    if not token or _LOG_TOKEN_RE.fullmatch(token) is None:
        return ""
    return token


def _flow_identity_suffix(flow: http.HTTPFlow) -> str:
    parts: list[str] = []
    subject = flow.metadata.get(FLOW_SUBJECT_KEY)
    if subject:
        parts.append(f"subject={subject}")
    # Live admission requests (sidecar and subject-admission) derive identity
    # strictly from the admitted handle and must never consult source IP (I6).
    if (
        not flow.metadata.get(FLOW_SUBJECT_REQUEST_KEY)
        and FLOW_REQUEST_HANDLE_KEY not in flow.metadata
    ):
        client_ip = _flow_client_ip(flow)
        if client_ip:
            parts.append(f"client_ip={client_ip}")
    return (" " + " ".join(parts)) if parts else ""


def _record_live_subject(flow: http.HTTPFlow, live: Any, handle: Any) -> None:
    if not flow.metadata.get(FLOW_SUBJECT_REQUEST_KEY):
        return
    with suppress(Exception):
        subject = _sanitize_log_token(live.resolve_subject_key(handle))
        if subject:
            flow.metadata[FLOW_SUBJECT_KEY] = subject


def _binding_log(flow: http.HTTPFlow, level: str, event: str, detail: str) -> None:
    if flow.metadata.get(FLOW_SUBJECT_REQUEST_KEY):
        detail = "credential proxy: subject request reason=" + event
    suffix = _flow_identity_suffix(flow)
    if suffix:
        detail += suffix
    _emit_credential_log(level, detail)


def _static_binding_headers(binding: dict[str, Any]) -> set[str]:
    return {
        name.lower()
        for header in (binding.get("headers") or [])
        if (name := header.get("name")) and header.get("value") is not None
    }


def _static_bindings_unchanged(
    expected: list[dict[str, Any]], fresh_vault: ActiveVault
) -> bool:
    by_name = {
        str(binding.get("name") or ""): binding
        for binding in fresh_vault.bindings
        if "exchange" not in binding
    }
    return all(
        by_name.get(str(binding.get("name") or "")) == binding
        for binding in expected
    )


def _binding_substitution_surfaces(binding: dict[str, Any]) -> set[str]:
    return {
        str(surface).lower()
        for substitution in (binding.get("substitutions") or [])
        for surface in (substitution.get("in") or [])
        if surface
    }


def _reject_ambiguous_binding(flow: http.HTTPFlow, detail: str | None = None) -> None:
    _reject_request(flow, b"credential binding ambiguous\n")
    _binding_log(
        flow,
        "warn",
        "ambiguous_binding",
        detail
        or (
            "credential proxy: ambiguous binding match for "
            f"{flow.request.method} {_request_host(flow)}{_request_path(flow)}"
        ),
    )


def _select_bindings(flow: http.HTTPFlow, vault: ActiveVault) -> list[dict[str, Any]]:
    matches: list[tuple[int, dict[str, Any]]] = []
    for binding in vault.bindings:
        ok, precedence = _binding_matches(flow, binding)
        if ok:
            matches.append((precedence, binding))
    if not matches:
        return []

    # Deterministic precedence order: exact host (2) before wildcard (1),
    # breaking ties by binding name.
    matches.sort(key=lambda item: (-item[0], str(item[1].get("name") or "")))
    selected = [binding for _, binding in matches]

    if len(selected) > 1:
        seen_headers: set[str] = set()
        seen_surfaces: set[str] = set()
        for binding in selected:
            headers = _static_binding_headers(binding)
            if headers & seen_headers:
                _reject_ambiguous_binding(flow)
                return []
            seen_headers.update(headers)

            surfaces = _binding_substitution_surfaces(binding)
            if surfaces & seen_surfaces:
                _reject_ambiguous_binding(flow)
                return []
            seen_surfaces.update(surfaces)

    return selected


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
            _binding_log(
                flow, "warn", "invalid_substitution",
                "credential proxy: rejected request after path substitution: "
                f"{flow.request.method} {_request_host(flow)} path=[REDACTED]",
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


def _connection_sni(flow: http.HTTPFlow) -> str | None:
    """Normalized SNI of the transport that carried this request, if any."""
    sni = getattr(getattr(flow, "client_conn", None), "sni", None)
    if not isinstance(sni, str) or not sni:
        return None
    return sni.lower().removesuffix(".")


_H1_AUTHORITY_FORBIDDEN_RE = re.compile(r"[\x00-\x20\x7f,@/\\?#%]")


def _parse_h1_authority(value: Any) -> tuple[str, int] | None:
    """Parse one Host/absolute-target authority into (normalized host, port).

    Strict HTTP/1 identity semantics for the live credential-bound mode: no
    whitespace, controls, comma, userinfo, slash, backslash, query, fragment
    or percent-encoded authority; at most one optional numeric port. DNS
    names normalize like vault hosts (ASCII lowercase with one optional
    trailing root dot). Returns None on any violation.
    """
    if type(value) is not str or not value or not value.isascii():
        return None
    if _H1_AUTHORITY_FORBIDDEN_RE.search(value):
        return None
    if value.count(":") > 1:
        return None
    host, separator, port_text = value.partition(":")
    port = 443
    if separator:
        if (
            not port_text
            or not port_text.isdecimal()
            or (port_text[0] == "0" and len(port_text) > 1)
            or len(port_text) > 5
        ):
            return None
        port = int(port_text)
        if not 1 <= port <= 65535:
            return None
    host = host.lower().removesuffix(".")
    if (
        not host
        or _active_vault_host_is_ip(host)
        or not _active_vault_host_is_fqdn(host)
    ):
        return None
    return host, port


def _live_h1_authority(flow: http.HTTPFlow) -> str | None:
    """Validate the HTTP/1 identity of one decrypted live request.

    Returns the normalized authority host, or None when the request must be
    denied: a non-HTTP/1 version (H2 has a separate raw-identity gate),
    CONNECT authority-form inside a decrypted stream, a non-443 target,
    missing/duplicate/malformed Host, or an absolute-target authority that
    disagrees with Host. Transparent-mode ``data.host`` may be an IP, so the
    transport destination is never compared here — only HTTP identity.
    """
    request = flow.request
    version = (request.http_version or "").upper()
    if version not in ("HTTP/1.0", "HTTP/1.1"):
        return None
    if (request.method or "").upper() == "CONNECT":
        return None
    if int(request.port or 0) != 443:
        return None
    try:
        hosts = request.headers.get_all("host")
    except Exception:  # noqa: BLE001 - a headers object without get_all denies
        return None
    if not isinstance(hosts, (list, tuple)) or len(hosts) != 1:
        return None
    parsed = _parse_h1_authority(hosts[0])
    if parsed is None:
        return None
    host, port = parsed
    if port != 443:
        return None
    # Prefer the raw wire authority bytes: request.authority applies IDNA
    # decoding, which would turn valid ASCII punycode into Unicode before
    # the strict ASCII parse. Fake requests without .data fall back to the
    # property value.
    authority: Any = None
    data = getattr(request, "data", None)
    raw_authority = getattr(data, "authority", None) if data is not None else None
    if isinstance(raw_authority, (bytes, bytearray, memoryview)):
        try:
            authority = bytes(raw_authority).decode("ascii")
        except UnicodeDecodeError:
            return None
    elif raw_authority is not None:
        authority = raw_authority
    elif data is None:
        authority = getattr(request, "authority", None)
    if authority:
        try:
            absolute = _parse_h1_authority(authority)
        except Exception:  # noqa: BLE001 - an unparseable authority denies
            return None
        if absolute is None or absolute != (host, port):
            return None
    return host


def _live_h2_authority(flow: http.HTTPFlow, identity: Any, adapter: Any) -> str | None:
    """Validate raw inbound identity, never transparent-mode rewritten identity."""
    request = flow.request
    if (request.http_version or "").upper() not in ("HTTP/2", "HTTP/2.0"):
        return None
    raw = identity.headers
    if type(raw) is not tuple or any(
        type(item) is not tuple or len(item) != 2
        or type(item[0]) is not bytes or type(item[1]) is not bytes
        for item in raw
    ):
        return None
    schemes = [value for name, value in raw if name == b":scheme"]
    authorities = [value for name, value in raw if name == b":authority"]
    methods = [value for name, value in raw if name == b":method"]
    hosts = [value for name, value in raw if name == b"host"]
    if (
        schemes != [b"https"] or len(authorities) != 1 or len(methods) != 1
        or methods[0].upper() == b"CONNECT" or not methods[0]
        or any(name == b":protocol" for name, _ in raw) or len(hosts) > 1
    ):
        return None
    target = adapter.http_layer.context.server
    address = target.address
    if (
        not address or type(address[1]) is not int or address[1] != 443
        or not target.tls or int(request.port or 0) != 443
    ):
        return None
    try:
        authority = _parse_h1_authority(authorities[0].decode("ascii"))
        host = _parse_h1_authority(hosts[0].decode("ascii")) if hosts else authority
    except UnicodeDecodeError:
        return None
    if authority is None or authority[1] != 443 or host != authority:
        return None
    return authority[0]


def _abort_live_h2(live: Any, flow: http.HTTPFlow, adapter: Any = None) -> None:
    """An invariant failure is never advertised as retryable refusal."""
    flow.metadata[FLOW_REJECTION_KEY] = True
    if adapter is not None:
        adapter.fail_closed()
    else:
        live._close_established(getattr(flow, "client_conn", None))
    if flow.killable:
        flow.kill()


def _requestheaders_live_h2(live: Any, flow: http.HTTPFlow) -> Awaitable[None] | None:
    client = getattr(flow, "client_conn", None)
    adapter = live.h2_for(client)
    token = live.token_for(client)
    if (
        adapter is None or token is None or adapter.token is not token
        or getattr(ctx.options, "ssl_insecure", False)
        or getattr(getattr(flow, "server_conn", None), "via", None) is not None
        or getattr(adapter.http_layer.context.server, "via", None) is not None
    ):
        _abort_live_h2(live, flow, adapter)
        return
    try:
        identity = adapter.consume_identity(flow.request)
        if identity is None:
            if adapter.request_is_closed(flow.request):
                if flow.killable:
                    flow.kill()
                return
            _abort_live_h2(live, flow, adapter)
            return
        admission = live.acquire_request(token, client_stream_id=identity.stream_id)
    except Exception:  # noqa: BLE001 - do not expose identity or snapshot errors
        _abort_live_h2(live, flow, adapter)
        return
    if admission.action != "allow" or admission.handle is None:
        if admission.reason in ("connection_fenced", "request_registry_exhausted"):
            adapter.refuse(identity, flow)
        else:
            _abort_live_h2(live, flow, adapter)
        return
    handle = admission.handle
    flow.metadata[FLOW_REQUEST_HANDLE_KEY] = handle
    _record_live_subject(flow, live, handle)
    try:
        adapter.bind_request(identity, flow, handle)
        authority = _live_h2_authority(flow, identity, adapter)
    except Exception:  # noqa: BLE001 - retain admitted handle until abnormal teardown
        _abort_live_h2(live, flow, adapter)
        return
    if authority is None or authority != _connection_sni(flow) or authority != token.sni:
        _abort_live_h2(live, flow, adapter)
        ctx.log.warn(f"credential proxy: rejected invalid H2 request authority{_flow_identity_suffix(flow)}")
        return
    flow.metadata[FLOW_H2_AUTHORITY_KEY] = authority
    try:
        vault = _live_snapshot_vault(admission.snapshot)
    except ActiveVaultLookupError:
        # This handle still protects downstream/upstream output obligations.
        _abort_live_h2(live, flow, adapter)
        ctx.log.warn(f"credential proxy: admitted snapshot failed validation; request denied{_flow_identity_suffix(flow)}")
        return
    _observe_tls_shadow(flow, vault)
    if vault is not None:
        return _apply_vault_binding(flow, vault, live=live)


def _finish_live_request(flow: http.HTTPFlow) -> None:
    """H1 hooks finish immediately; H2 retains transport output obligations."""
    handle = flow.metadata.get(FLOW_REQUEST_HANDLE_KEY)
    live = (
        _subject_admission_runtime()
        if flow.metadata.get(FLOW_SUBJECT_REQUEST_KEY) else _live_runtime
    )
    if handle is not None and live is not None:
        if getattr(handle, "client_stream_id", None) is not None:
            adapter = live.h2_for(getattr(flow, "client_conn", None))
            if adapter is not None:
                adapter.application_done(flow)
            else:
                _abort_live_h2(live, flow)
            return
        flow.metadata.pop(FLOW_REQUEST_HANDLE_KEY, None)
        live.finish_request(handle)


def _observe_tls_shadow(
    flow: http.HTTPFlow, vault: ActiveVault | None, *, lookup_failed: bool = False
) -> None:
    if not _tls_shadow_enabled:
        return
    try:
        if flow.request.scheme != "https" or flow.request.port != 443:
            return
        from tls_shadow import project

        sni = getattr(getattr(flow, "client_conn", None), "sni", None)
        outcome = project(
            sni, None if vault is None else vault.bindings, lookup_failed=lookup_failed
        )
        if _fast_sandbox_mode_enabled and vault is None and not lookup_failed:
            # Fast Sandbox 404 also means unknown source identity, not just no vault.
            outcome = "unknown_subject_or_vault"
        # Fixed vocabulary only: no hostname, revision, subject, path, or secret.
        ctx.log.warn("credential proxy: tls-shadow " + outcome)
    except Exception:  # noqa: BLE001 - diagnostics must never change traffic
        with suppress(Exception):
            ctx.log.warn("credential proxy: tls-shadow observer_error")


def _requestheaders_pull(flow: http.HTTPFlow) -> Awaitable[None] | None:
    """Pull-mode processing; returns an awaitable only when an exchange
    binding defers its resolve I/O to the caller."""
    try:
        vault = _load_active_vault(_flow_client_ip(flow))
    except ActiveVaultLookupError as exc:
        _observe_tls_shadow(flow, None, lookup_failed=True)
        _emit_credential_log(
            "warn", f"credential proxy: {exc}; request denied{_flow_identity_suffix(flow)}"
        )
        _reject_request(
            flow,
            b"credential proxy unavailable\n",
            status_code=503,
        )
        return None
    _observe_tls_shadow(flow, vault)
    if vault is None:
        return None
    if vault.subject:
        flow.metadata[FLOW_SUBJECT_KEY] = vault.subject
    return _apply_vault_binding(flow, vault)


async def requestheaders(flow: http.HTTPFlow) -> None:
    """Credential proxy phase 1: binding match and request metadata rewrite.

    Header injection must happen here, before the upstream connection is
    made: with ``stream_large_bodies=1m`` the ``request`` hook fires only
    after a body above 1 MiB has been streamed upstream.

    In live credential-bound mode the vault comes from the admitted decision
    snapshot (see :func:`_requestheaders_live`) instead of the pull cache.

    The hook is async because mitmproxy awaits it before the request
    continues: exchange bindings defer resolve I/O to the single await
    below; static bindings stay inline and never suspend.
    """
    subject = _subject_admission_runtime()
    if subject is not None:
        flow.metadata[FLOW_SUBJECT_REQUEST_KEY] = True
        client = getattr(flow, "client_conn", None)
        version = (flow.request.http_version or "").upper()
        if (getattr(client, "alpn", None) == b"h2"
                or version in ("HTTP/2", "HTTP/2.0")
                or ((flow.request.scheme or "").lower() == "https" and version != "HTTP/1.1")):
            flow.metadata[FLOW_REJECTION_KEY] = True
            if flow.killable:
                flow.kill()
            ctx.log.warn(
                "credential proxy: request denied reason=subject_protocol_unsupported"
                f"{_flow_identity_suffix(flow)}"
            )
            return
        deferred = _requestheaders_live(subject, flow)
        if deferred is not None:
            await deferred
        if _flow_rejected(flow):
            # Subject denials terminate transport, including streamed requests:
            # discard the shared path's 403/503 response (used by sidecar mode).
            _finish_live_request(flow)
            flow.response = None
            if flow.killable:
                flow.kill()
        return
    live = _live_runtime
    if live is not None:
        deferred = _requestheaders_live(live, flow)
        if deferred is not None:
            await deferred
        return
    deferred = _requestheaders_pull(flow)
    if deferred is not None:
        await deferred


def _requestheaders_live(live: Any, flow: http.HTTPFlow) -> Awaitable[None] | None:
    """Authorize one request against the admitted revision snapshot.

    Only HTTPS carries live admission; plain HTTP receives no injected
    credentials. Tracked H2 must first validate raw identity even if transparent
    rewriting changed the visible scheme. Every request on a decrypted connection
    must acquire request admission; a fenced connection (removed host, drained
    revision) is rejected and never falls back to the retired snapshot.
    """
    client = getattr(flow, "client_conn", None)
    if (
        getattr(client, "alpn", None) == b"h2"
        or (flow.request.http_version or "").upper() in ("HTTP/2", "HTTP/2.0")
    ):
        # Genuine cleartext H2 remains outside credential admission. Scheme
        # alone cannot establish that: tracked/decrypted H2 must validate its
        # captured raw identity even after another hook rewrites the scheme.
        if (getattr(client, "tls", None) is False
                and getattr(client, "alpn", None) is None
                and live.token_for(client) is None and live.h2_for(client) is None
                and (flow.request.scheme or "").lower() == "http"):
            return
        return _requestheaders_live_h2(live, flow)
    if (flow.request.scheme or "").lower() != "https":
        return
    # Per-request defense: insecure upstream verification must never be
    # effective in live mode. Deny instead of injecting into or tunneling a
    # bound request toward an unverified origin.
    if getattr(ctx.options, "ssl_insecure", False):
        _reject_request(flow, b"credential proxy unavailable\n", status_code=503)
        ctx.log.warn(f"credential proxy: request denied reason=ssl_insecure{_flow_identity_suffix(flow)}")
        return
    token = live.token_for(getattr(flow, "client_conn", None))
    admission = None if token is None else live.acquire_request(token)
    handle = None
    if admission is not None and admission.action == "allow":
        handle = admission.handle
        flow.metadata[FLOW_REQUEST_HANDLE_KEY] = handle
        _record_live_subject(flow, live, handle)
    if handle is None:
        reason = "no_connection_admission" if admission is None else admission.reason
        status_code = 403 if reason == "connection_fenced" else 503
        _reject_request(flow, b"credential proxy unavailable\n", status_code=status_code)
        if flow.response is not None:
            # Fenced transports stop accepting new requests: tell HTTP/1.1
            # keepalive clients this connection is finished.
            flow.response.headers["connection"] = "close"
        _emit_credential_log(
            "warn", f"credential proxy: request denied reason={reason}{_flow_identity_suffix(flow)}"
        )
        return

    # HTTP/1 identity gate, applied to every decrypted request before any
    # binding selection: the validated authority must equal both the
    # connection SNI and the immutable admission token SNI, so a credential
    # can only reach the verified TLS peer. Cross-authority requests on
    # out-of-scope paths are denied just the same.
    authority_host = _live_h1_authority(flow)
    connection_sni = _connection_sni(flow)
    if (
        authority_host is None
        or connection_sni is None
        or authority_host != connection_sni
        or authority_host != token.sni
    ):
        _reject_request(
            flow,
            b"request authority does not match the intercepted destination\n",
            status_code=403,
        )
        ctx.log.warn(f"credential proxy: rejected invalid or cross-authority request{_flow_identity_suffix(flow)}")
        return

    try:
        vault = _live_snapshot_vault(admission.snapshot)
    except ActiveVaultLookupError:
        flow.metadata.pop(FLOW_REQUEST_HANDLE_KEY, None)
        live.finish_request(handle)
        _reject_request(flow, b"credential proxy unavailable\n", status_code=503)
        ctx.log.warn(f"credential proxy: admitted snapshot failed validation; request denied{_flow_identity_suffix(flow)}")
        return
    _observe_tls_shadow(flow, vault)
    if vault is None:
        return
    return _apply_vault_binding(flow, vault, live=live)


def _live_snapshot_vault(snapshot: Any) -> ActiveVault | None:
    """Convert an admitted immutable snapshot into the binding view.

    Returns None for an authoritative snapshot without rendered bindings
    (no credential can match). Raises ActiveVaultLookupError for any payload
    that does not exactly reconstruct the validated envelope, which fails the
    request closed instead of forwarding it uncredentialed by accident.

    Only call after successful request admission and authority checks. The
    receiver has already verified the immutable payload's digest; memoizing
    its parsed view never caches the admission or rehashes the payload.
    """
    global _live_vault_cache
    identity = snapshot.revision
    cached = _live_vault_cache
    if cached is not None and cached[0] == identity:
        return cached[1]
    # A miss drops the owner's old view even if decoding or validation fails.
    _live_vault_cache = None
    try:
        payload = json.loads(snapshot.payload)
    except Exception:  # noqa: BLE001 - payload bytes may contain credentials
        raise ActiveVaultLookupError("admitted snapshot could not be decoded") from None
    if not isinstance(payload, dict):
        raise ActiveVaultLookupError("admitted snapshot must be an object")
    revision = snapshot.revision.vault_revision
    if type(revision) is not int or payload.get("vaultRevision") != revision:
        raise ActiveVaultLookupError("admitted snapshot revision disagreement")
    state = payload.get("state")
    bindings = payload.get("fullRenderedBindings")
    if state == "active-empty":
        if bindings:
            raise ActiveVaultLookupError("admitted snapshot state disagreement")
        _live_vault_cache = (identity, None)
        return None
    if state != "active" or not isinstance(bindings, list) or not bindings:
        raise ActiveVaultLookupError("admitted snapshot has no rendered bindings")
    redactions = payload.get("redactions", [])
    if not isinstance(redactions, list):
        raise ActiveVaultLookupError("admitted snapshot redactions are invalid")
    vault = _parse_active_vault(
        {"revision": revision, "bindings": bindings, "redactions": redactions}
    )
    _live_vault_cache = (identity, vault)
    return vault


def _reject_ambiguous_binding_path(
    flow: http.HTTPFlow, vault: ActiveVault
) -> bool:
    """Shared path gate for static and exchange injection: reject ambiguous
    or boundary-crossing encodings so a secret never reaches a scope the
    canonical path does not match. Returns True when rejected."""
    raw_path = flow.request.path or "/"
    if _path_is_ambiguous(raw_path, allow_encoded_slash=True):
        _reject_request(flow, b"request path contains ambiguous segments\n")
        _binding_log(
            flow, "warn", "ambiguous_path",
            "credential proxy: rejected request with ambiguous path: "
            f"{flow.request.method} {_request_host(flow)}{_request_path(flow)}",
        )
        return True
    if _path_decoding_changes_binding(flow, vault):
        _reject_request(flow, b"request path contains ambiguous segments\n")
        _binding_log(
            flow, "warn", "ambiguous_path",
            "credential proxy: rejected request whose percent-decoding crosses "
            "the credential binding boundary: "
            f"{flow.request.method} {_request_host(flow)}{_request_path(flow)}",
        )
        return True
    return False


def _apply_vault_binding(
    flow: http.HTTPFlow, vault: ActiveVault, live: Any | None = None
) -> Awaitable[None] | None:
    """Shared binding selection, ambiguity checks and credential injection.

    Returns an awaitable when one or more matched bindings require exchange
    resolution (their resolve I/O must complete before the request continues)."""
    # Requests outside credential binding scope are ordinary egress traffic.
    # Leave them untouched, including paths whose encoding would be ambiguous
    # for credential injection, because no secret is at risk.
    bindings = _select_bindings(flow, vault)
    if not bindings:
        return None

    if _reject_ambiguous_binding_path(flow, vault):
        return None

    exchange_indices = [
        index for index, binding in enumerate(bindings) if "exchange" in binding
    ]
    if exchange_indices:
        return _apply_exchange_bindings(flow, vault, bindings, exchange_indices, live)

    _commit_bindings(flow, vault, bindings, {})
    return None


def _apply_exchange_bindings(
    flow: http.HTTPFlow,
    vault: ActiveVault,
    bindings: list[dict[str, Any]],
    exchange_indices: list[int],
    live: Any | None,
) -> Awaitable[None] | None:
    """Gate matched exchange bindings, then resolve before committing."""
    sni = _connection_sni(flow)
    if sni is None:
        # Without a connection SNI the egress SNI gate can never pass.
        _reject_request(
            flow, b"credential resolution unavailable\n", status_code=407
        )
        ctx.log.warn(
            "credential proxy: exchange binding without connection sni was denied"
            f"{_flow_identity_suffix(flow)}"
        )
        return None
    return _resolve_exchange_bindings(
        flow, vault, bindings, exchange_indices, sni, live
    )


async def _resolve_exchange_bindings(
    flow: http.HTTPFlow,
    vault: ActiveVault,
    bindings: list[dict[str, Any]],
    exchange_indices: list[int],
    sni: str,
    live: Any | None = None,
) -> None:
    """Resolve all matched exchange bindings on the active socket and commit.

    Resolvers see only the original unmutated client request (never secrets
    from sibling static or exchange bindings). I/O runs in worker threads
    behind the semaphore so slow resolvers never block the loop; any failure
    fails closed (407/kill) and any header collision across composed bindings
    fails closed (403/kill) before mutating the request."""
    identity_suffix = _flow_identity_suffix(flow)
    path = CREDENTIAL_RESOLVE_PATH
    client_ip: str | None = None
    if live is not None:
        try:
            handle = flow.metadata[FLOW_REQUEST_HANDLE_KEY]
            revision = handle.snapshot.revision
            admission = {
                "subjectKey": live.resolve_subject_key(handle)
                if flow.metadata.get(FLOW_SUBJECT_REQUEST_KEY) else "",
                "revision": {
                    "controlGeneration": revision.control_generation,
                    "subjectGeneration": revision.subject_generation,
                    "decisionEpoch": revision.decision_epoch,
                    "vaultRevision": revision.vault_revision,
                    "policyEpoch": revision.policy_epoch,
                    "digest": revision.digest,
                },
            }
            path += "?admission=" + quote(json.dumps(admission, separators=(",", ":")), safe="")
        except Exception:  # noqa: BLE001 - no fallback from missing admission
            _reject_request(flow, b"credential resolution unavailable\n", status_code=407)
            ctx.log.warn(
                "credential proxy: exchange resolve denied reason=missing_admission"
                f"{identity_suffix}"
            )
            return
    elif _fast_sandbox_mode_enabled:
        client_ip = _flow_client_ip(flow)
        if not client_ip:
            _reject_request(
                flow, b"credential resolution unavailable\n", status_code=407
            )
            ctx.log.warn(
                "credential proxy: exchange resolve denied reason=missing_client_ip"
                f"{identity_suffix}"
            )
            return
        path = f"{CREDENTIAL_RESOLVE_PATH}?clientIp={quote(client_ip)}"
    socket_path = (
        os.environ.get(CREDENTIAL_PROXY_SOCKET_ENV, "").strip()
        or DEFAULT_CREDENTIAL_PROXY_SOCKET
    )
    budget = _resolve_budget_seconds()
    deadline = time.monotonic() + budget
    first_name = bindings[exchange_indices[0]].get("name")
    try:
        request_headers = _resolve_request_headers(flow)
    except CredentialResolveError as error:
        _reject_request(
            flow, b"credential resolution unavailable\n", status_code=407
        )
        _emit_credential_log(
            "warn",
            f"credential proxy: exchange resolve failed binding={first_name} "
            f"reason={error}{identity_suffix}",
        )
        return
    except Exception:  # noqa: BLE001 - unexpected failures fail closed too
        _reject_request(
            flow, b"credential resolution unavailable\n", status_code=407
        )
        _emit_credential_log(
            "warn",
            f"credential proxy: exchange resolve failed binding={first_name} "
            f"reason=unexpected{identity_suffix}",
        )
        return

    request_frame = {
        "scheme": (flow.request.scheme or "").lower(),
        "host": _request_host(flow),
        "port": _request_port(flow),
        "method": (flow.request.method or "").upper(),
        "path": flow.request.path or "/",
        "headers": request_headers,
    }
    exchange_names = [str(bindings[index].get("name") or "") for index in exchange_indices]
    exchange_index_set = set(exchange_indices)
    sibling_static_bindings = [
        binding for idx, binding in enumerate(bindings) if idx not in exchange_index_set
    ]
    sibling_names = [str(binding.get("name") or "") for binding in sibling_static_bindings]
    is_group = len(bindings) > 1
    if is_group:
        log_binding_name = ",".join(exchange_names)
        payload: dict[str, Any] = {
            "bindings": exchange_names,
            "sni": sni,
            "request": request_frame,
        }
        if sibling_names:
            payload["siblingBindings"] = sibling_names
    else:
        log_binding_name = exchange_names[0]
        payload = {
            "binding": exchange_names[0],
            "sni": sni,
            "request": request_frame,
        }
    # Same serialization the RPC sends, so per-header caps cannot hide
    # an oversized selection (or path) from the endpoint's 64KiB bound.
    if len(json.dumps(payload).encode("utf-8")) > MAX_RESOLVE_REQUEST_BYTES:
        _reject_request(
            flow, b"credential resolution unavailable\n", status_code=407
        )
        _emit_credential_log(
            "warn",
            f"credential proxy: exchange resolve failed binding={log_binding_name} "
            f"reason=resolve_payload_too_large{identity_suffix}",
        )
        return

    async def _call_resolve(req_payload: dict[str, Any]) -> dict[str, Any]:
        async with _resolve_limiter():
            # The deadline covers semaphore and executor queueing too, not
            # just the socket: a queued resolve fails within the budget
            # instead of stacking timeout batches behind a slow resolver.
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise CredentialResolveError("deadline")
            return await asyncio.wait_for(
                asyncio.to_thread(
                    _resolve_exchange_http,
                    socket_path,
                    remaining,
                    path,
                    req_payload,
                ),
                timeout=remaining,
            )

    try:
        response = await _call_resolve(payload)
        if is_group:
            raw_decisions = response.get("decisions") if isinstance(response, dict) else None
            if not isinstance(raw_decisions, list) or len(raw_decisions) != len(exchange_indices):
                raise CredentialResolveError("invalid_response")
            decisions_by_idx = {
                index: decision
                for index, decision in zip(exchange_indices, raw_decisions)
            }
        else:
            decisions_by_idx = {exchange_indices[0]: response}
        if live is not None:
            for decision in decisions_by_idx.values():
                if decision["revision"] != vault.revision:
                    raise CredentialResolveError("revision_mismatch")
        elif sibling_static_bindings and any(
            decision["revision"] != vault.revision
            for decision in decisions_by_idx.values()
        ):
            # A vault mutation occurred between _load_active_vault and Go's
            # group capture; verify the rendered sibling static bindings are
            # still identical in the latest active snapshot (unrelated mutations
            # remain allowed).
            fresh_vault = await asyncio.to_thread(_load_active_vault, client_ip)
            if (
                fresh_vault is None
                or (vault.subject and fresh_vault.subject != vault.subject)
                or not _static_bindings_unchanged(sibling_static_bindings, fresh_vault)
            ):
                raise CredentialResolveError("http_409")
    except ActiveVaultLookupError:
        _reject_request(
            flow, b"credential resolution unavailable\n", status_code=407
        )
        _emit_credential_log(
            "warn",
            f"credential proxy: exchange resolve failed binding={log_binding_name} "
            f"reason=vault_recheck_failed{identity_suffix}",
        )
        return
    except CredentialResolveError as error:
        _reject_request(
            flow, b"credential resolution unavailable\n", status_code=407
        )
        _emit_credential_log(
            "warn",
            f"credential proxy: exchange resolve failed binding={log_binding_name} "
            f"reason={error}{identity_suffix}",
        )
        return
    except asyncio.TimeoutError:
        _reject_request(
            flow, b"credential resolution unavailable\n", status_code=407
        )
        _emit_credential_log(
            "warn",
            f"credential proxy: exchange resolve failed binding={log_binding_name} "
            f"reason=deadline{identity_suffix}",
        )
        return
    except Exception:  # noqa: BLE001 - unexpected failures fail closed too
        _reject_request(
            flow, b"credential resolution unavailable\n", status_code=407
        )
        _emit_credential_log(
            "warn",
            f"credential proxy: exchange resolve failed binding={log_binding_name} "
            f"reason=unexpected{identity_suffix}",
        )
        return

    # Enforce disjoint injected header names across all static and resolved
    # exchange bindings before mutating the request.
    seen_headers: set[str] = set()
    for binding in bindings:
        if "exchange" not in binding:
            seen_headers.update(_static_binding_headers(binding))
    for index in exchange_indices:
        for header in decisions_by_idx[index]["headers"]:
            lowered = header["name"].lower()
            if lowered in seen_headers:
                _reject_ambiguous_binding(
                    flow,
                    "credential proxy: ambiguous binding header collision "
                    f"binding={','.join(str(b.get('name') or '') for b in bindings)} "
                    f"host={_request_host(flow)} method={flow.request.method}",
                )
                return
            seen_headers.add(lowered)

    _commit_bindings(flow, vault, bindings, decisions_by_idx)


def _commit_bindings(
    flow: http.HTTPFlow,
    vault: ActiveVault,
    bindings: list[dict[str, Any]],
    decisions_by_idx: dict[int, dict[str, Any]],
) -> None:
    """Apply substitutions, inject static + resolved headers, and persist redactions.

    For exchange decisions, variants of every injected value are re-derived
    (R9) and injected names always join the suppress set (D7), so redaction
    never depends on resolver output completeness."""
    substituted_surfaces: list[str] = []
    for binding in bindings:
        if "exchange" in binding:
            continue
        substituted_surfaces.extend(_apply_requestheaders_substitutions(flow, binding))
        if _flow_rejected(flow):
            return

    injected_names: list[str] = []
    exchange_injected_names: list[str] = []
    for index, binding in enumerate(bindings):
        if "exchange" in binding:
            for header in decisions_by_idx[index]["headers"]:
                name = header["name"]
                # Delete-then-set, as static injection: the resolved value must be
                # the only one.
                if name in flow.request.headers:
                    del flow.request.headers[name]
                flow.request.headers[name] = header["value"]
                injected_names.append(name)
                exchange_injected_names.append(name)
        else:
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

    flow.metadata[FLOW_BINDING_KEY] = bindings[0]
    flow.metadata[FLOW_BINDINGS_KEY] = list(bindings)
    # Persist the redactions of the matched revision and any exchange decisions:
    # body substitutions run later in the request hook, and reloading the vault
    # there could return a different revision after a runtime mutation, leaving
    # substituted credentials unredactable in response headers.
    redactions = list(vault.redactions)
    has_exchange = bool(decisions_by_idx)
    if has_exchange:
        suppress: set[str] = set()
        for index in sorted(decisions_by_idx):
            decision = decisions_by_idx[index]
            redactions.extend(decision["redactions"])
            for header in decision["headers"]:
                redactions.extend(
                    _active_snapshot_substitution_redaction_variants(header["value"])
                )
            suppress.update(decision["suppressHeaders"])
        suppress.update(name.lower() for name in exchange_injected_names)
        flow.metadata[FLOW_VAULT_SUPPRESS_KEY] = sorted(suppress)
    flow.metadata[FLOW_VAULT_REDACTIONS_KEY] = redactions

    binding_names = ",".join(str(b.get("name") or "") for b in bindings)
    has_static = any("exchange" not in b for b in bindings)
    if has_exchange or injected_names or substituted_surfaces:
        flow.metadata[FLOW_REDACTIONS_KEY] = list(redactions)
        revisions: list[str] = []
        if has_static:
            revisions.append(str(vault.revision))
        for index in sorted(decisions_by_idx):
            rev = str(decisions_by_idx[index]["revision"])
            if rev not in revisions:
                revisions.append(rev)
        parts = [
            f"credential proxy: applied binding={binding_names}",
            f"revision={','.join(revisions)}",
            f"host={_request_host(flow)}",
            f"method={flow.request.method}",
            f"headers={','.join(injected_names)}",
        ]
        if has_static and (not has_exchange or substituted_surfaces):
            parts.append(
                f"substitutions={','.join(sorted(set(substituted_surfaces)))}"
            )
        if has_exchange:
            parts.append("exchange=1")
        _binding_log(flow, "info", "binding_processed", " ".join(parts))
    elif any(
        _has_substitutions_on(binding, {"path", "query", "header"})
        for binding in bindings
        if "exchange" not in binding
    ):
        _binding_log(
            flow,
            "info",
            "binding_processed",
            "credential proxy: substitution miss binding="
            f"{binding_names} revision={vault.revision} "
            f"host={_request_host(flow)} method={flow.request.method}",
        )


def request(flow: http.HTTPFlow) -> None:
    """Credential proxy phase 2: body substitutions.

    Needs the full body, so it cannot run in ``requestheaders``. Streamed
    requests are skipped: the body was already forwarded upstream and is no
    longer available or modifiable.
    """
    bindings = flow.metadata.get(FLOW_BINDINGS_KEY)
    if bindings is None:
        single = flow.metadata.get(FLOW_BINDING_KEY)
        bindings = [single] if single is not None else []
    if not bindings:
        return
    if getattr(flow.request, "stream", False):
        return

    applied_names: list[str] = []
    missed_names: list[str] = []
    for binding in bindings:
        if "exchange" in binding:
            continue
        applied = _apply_body_substitutions(flow, binding.get("substitutions") or [])
        if applied:
            applied_names.append(str(binding.get("name") or ""))
        elif _has_substitutions_on(binding, {"body"}):
            missed_names.append(str(binding.get("name") or ""))

    if applied_names:
        if FLOW_REDACTIONS_KEY not in flow.metadata:
            flow.metadata[FLOW_REDACTIONS_KEY] = list(
                flow.metadata.get(FLOW_VAULT_REDACTIONS_KEY, [])
            )
        _binding_log(
            flow,
            "info",
            "body_processed",
            "credential proxy: applied body substitutions binding="
            f"{','.join(applied_names)} host={_request_host(flow)} "
            f"method={flow.request.method}",
        )
    elif FLOW_REDACTIONS_KEY not in flow.metadata and missed_names:
        _binding_log(
            flow,
            "info",
            "body_processed",
            "credential proxy: substitution miss binding="
            f"{','.join(missed_names)} host={_request_host(flow)} "
            f"method={flow.request.method}",
        )


def responseheaders(flow: http.HTTPFlow) -> None:
    if flow.response is None:
        return
    _suppress_response_headers(flow)
    _redact_response_headers(flow)
    content_type = flow.response.headers.get("content-type", "").lower()
    transfer_encoding = flow.response.headers.get("transfer-encoding", "").lower()
    if "text/event-stream" in content_type or "chunked" in transfer_encoding:
        flow.response.stream = True


def response(flow: http.HTTPFlow) -> None:
    """Record application completion; H2 output and upgraded transports persist."""
    if (
        FLOW_REQUEST_HANDLE_KEY in flow.metadata
        and flow.response is not None
        and flow.response.status_code == 101
    ):
        # WebSocket and raw protocol upgrades outlive the HTTP response hook.
        # Keep the handle so rotation assigns a retirement deadline even if
        # the host stays bound. client_disconnected releases it with the token.
        return
    _finish_live_request(flow)


def error(flow: http.HTTPFlow) -> None:
    """Record failure; H2 retains its handle until transport obligations end."""
    _finish_live_request(flow)


def client_connected(client_conn: Any) -> None:
    """Index every proxied subject client, decrypted or opaque, at connect."""
    subject = _subject_admission_runtime()
    if subject is not None:
        try:
            subject.client_connected(client_conn)
        except Exception:  # noqa: BLE001 - an unindexed client must not proceed
            client_conn.error = "connection fenced"


def client_disconnected(client_conn: Any) -> None:
    """Release the transport's live admission on confirmed disconnect."""
    subject = _subject_admission_runtime()
    if subject is not None:
        subject.client_disconnected(client_conn)
    live = _live_runtime
    if live is not None:
        live.client_disconnected(client_conn)


def _suppress_response_headers(flow: http.HTTPFlow) -> None:
    """Delete by-name suppressed response headers; the set was validated at
    resolve time and every occurrence is removed."""
    suppress = flow.metadata.get(FLOW_VAULT_SUPPRESS_KEY)
    if not suppress or flow.response is None:
        return
    suppressed = {name.lower() for name in suppress}
    for name in {name.lower() for name, _ in flow.response.headers.items()}:
        if name in suppressed:
            del flow.response.headers[name]


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
