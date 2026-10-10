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

"""Live credential-bound admission runtime for the OSEP-0023 sidecar.

system.py delegates to this module when the Go launcher hands off the live
admission bundle (decrypted-connection capacity, admitted-request capacity,
drain timeout) next to the revision IPC session. Without that bundle the
addon keeps the installation-only receiver and the legacy pull-based path.

This module owns only transport bookkeeping: the connection<->token map, the
drain thread, and fail-closed connection termination. All credential policy
(binding selection, injection, redaction) stays in system.py, which calls
``token_for``, ``acquire_request`` and ``finish_request`` from its hooks.

Security invariants (OSEP-0023 phase 3, sidecar experiment):
- A TLS decision is derived only from the installed immutable snapshot; an
  unknown or bootstrapping state denies instead of falling back to decrypt-all
  or to dynamic pass-through.
- A decrypted connection exists only with a registry admission; a request on a
  connection without one is rejected, never silently forwarded with credentials.
- Removed hosts fence new requests on tracked connections immediately; the
  bounded retirement deadline force-closes the transport at expiry. No fence is
  reversible, so remove/re-add cannot resurrect a retired connection.
"""

from __future__ import annotations

import os
import re
import threading
from contextlib import suppress
from typing import Any

from mitmproxy import ctx

from revision_publication import LiveReceiver
from tls_registry import AdmissionResult, AdmissionToken, RequestHandle

# Fixed-vocabulary diagnostics; never a host, revision, subject or secret.
_DRAIN_INTERVAL_SECONDS = 0.5
_DRAIN_PAGE_LIMIT = 64
_LIVE_RUNTIME_ERROR = "credential proxy: invalid revision runtime configuration"

_LIVE_ENV = {
    "tls_capacity": "OPENSANDBOX_EGRESS_REVISION_TLS_CAPACITY",
    "request_capacity": "OPENSANDBOX_EGRESS_REVISION_REQUEST_CAPACITY",
    "drain_timeout": "OPENSANDBOX_EGRESS_REVISION_DRAIN_TIMEOUT_SECONDS",
}


def _fatal() -> None:
    # Same process-level fence as system.py: a listener without a coherent
    # live admission runtime must not serve traffic.
    raise SystemExit(_LIVE_RUNTIME_ERROR) from None


def _env_int(name: str, value: str, minimum: int, maximum: int) -> int:
    if (
        not value
        or not value.isascii()
        or not value.isdecimal()
        or (value[0] == "0" and len(value) > 1)
    ):
        _fatal()
    number = int(value)
    if number < minimum or number > maximum:
        _fatal()
    return number


def _live_configuration() -> tuple[int, int, int] | None:
    """Return (tls capacity, request capacity, drain timeout) or None."""
    present = {key for key, name in _LIVE_ENV.items() if name in os.environ}
    if not present:
        return None
    if present != set(_LIVE_ENV):
        _fatal()
    values = {key: os.environ[name] for key, name in _LIVE_ENV.items()}
    tls_capacity = _env_int("tls_capacity", values["tls_capacity"], 1, 1 << 20)
    request_capacity = _env_int("request_capacity", values["request_capacity"], 1, 1 << 20)
    drain_timeout = _env_int("drain_timeout", values["drain_timeout"], 1, 300)
    return tls_capacity, request_capacity, drain_timeout


def live_admission_requested() -> bool:
    """True when the launcher supplied any live admission budget variable."""
    return any(name in os.environ for name in _LIVE_ENV.values())


class LiveRuntime:
    """Transport bookkeeping for one live credential-bound mitmdump process.

    One instance serves exactly one sidecar generation; a replacement process
    builds a fresh receiver, registry and runtime.
    """

    def __init__(self, receiver: LiveReceiver, identity: tuple[str, str]) -> None:
        self._receiver = receiver
        self.registry = receiver.registry
        self.identity = identity
        self._lock = threading.Lock()
        # id(client_conn) -> (token, client_conn). Holding the connection
        # reference keeps the id unique while tracked.
        self._connections: dict[int, tuple[AdmissionToken, Any]] = {}
        self._serials: dict[int, int] = {}
        # Captured inside the event loop at the first tracked handshake; used
        # by the drain thread to schedule thread-safe transport closure.
        self._loop: Any = None
        self._proxyserver: Any = None
        self._drain_stop = threading.Event()
        self._drain_thread = threading.Thread(
            target=self._drain_loop, name="credential-bound-drain", daemon=True
        )
        self._closed = False

    def start(self) -> None:
        self._drain_thread.start()

    def _capture_loop(self) -> None:
        """Record the proxy event loop and connection registry (in-loop only)."""
        if self._loop is not None:
            return
        try:
            import asyncio

            self._loop = asyncio.get_running_loop()
        except Exception:  # noqa: BLE001 - fake or non-loop contexts
            self._loop = None
        try:
            self._proxyserver = ctx.master.addons.get("proxyserver")
        except Exception:  # noqa: BLE001 - fake or non-loop contexts
            self._proxyserver = None

    # -- TLS decision path -------------------------------------------------

    def static_ignored(self, sni: str | None) -> bool:
        """Legacy operator regex pass-through keeps its existing precedence."""
        if not sni:
            return False
        for pattern in ctx.options.ignore_hosts or []:
            try:
                if re.search(pattern, sni):
                    return True
            except re.error:
                pass
        return False

    def admit_tls(self, sni: str | None, ech_hidden: bool = False) -> AdmissionResult:
        """Classify one ClientHello against the installed snapshot.

        ``ech_hidden`` is set by the caller only after inspecting the actual
        ClientHello extension list: an ECH-bearing (or GREASE-ECH) connection
        is passed through opaquely — the byte stream reaches the outer-SNI
        origin untouched, never decrypted and never credential-bound.
        Exact-hostname bindings keep hidden inner names out of the decision
        set, and injection still requires the full request binding match
        plus SNI/authority agreement.
        """
        return self.registry.admit(
            identity=self.identity,
            sni=sni,
            ech_hidden=ech_hidden,
            static_passthrough=(),
        )

    def track(self, token: AdmissionToken | None, client_conn: Any) -> bool:
        """Bind one admitted decrypt decision to its client connection."""
        if token is None or client_conn is None:
            return False
        self._capture_loop()
        with self._lock:
            if self._closed:
                return False
            key = id(client_conn)
            previous = self._connections.get(key)
            self._connections[key] = (token, client_conn)
            self._serials[token.serial] = key
        if previous is not None and previous[0] is not token:
            # A repeated handshake on one transport replaces its admission.
            self.registry.release(previous[0])
        return True

    def token_for(self, client_conn: Any) -> AdmissionToken | None:
        if client_conn is None:
            return None
        with self._lock:
            entry = self._connections.get(id(client_conn))
            return None if entry is None else entry[0]

    # -- Request admission path -------------------------------------------

    def acquire_request(self, token: AdmissionToken | None) -> Any:
        """Delegate to the registry; see BoundConnectionRegistry.acquire_request."""
        return self.registry.acquire_request(token)

    def finish_request(self, handle: RequestHandle | None) -> None:
        """Idempotent terminal cleanup for one admitted request."""
        self.registry.finish_request(handle)

    # -- Connection lifecycle ----------------------------------------------

    def client_disconnected(self, client_conn: Any) -> None:
        """Release the connection's admission on confirmed transport close."""
        if client_conn is None:
            return
        with self._lock:
            entry = self._connections.pop(id(client_conn), None)
            if entry is not None:
                self._serials.pop(entry[0].serial, None)
        if entry is not None:
            self.registry.release(entry[0])

    # -- Drain -------------------------------------------------------------

    def _drain_loop(self) -> None:
        while not self._drain_stop.wait(_DRAIN_INTERVAL_SECONDS):
            try:
                self.drain_sweep_once()
            except Exception:  # noqa: BLE001 - diagnostics must never kill the thread
                with suppress(Exception):
                    ctx.log.warn("credential proxy: credential-bound drain error")

    def drain_sweep_once(self) -> int:
        """Close live transports whose retirement deadline expired.

        Each sweep restarts at serial zero (lower serials can expire later)
        and pages forward with the after_serial cursor, so every expired
        transport is attempted once per sweep — including targets past the
        first page. The sweep terminates on an empty or short page, so a
        stuck target cannot spin this sweep; a failed close is retried by the
        next sweep instead. Closing is best-effort here: the disconnect hook
        performs the release, and a scheduled close never counts as released.
        """
        attempts = 0
        cursor = 0
        while True:
            tokens = self.registry.expired_connections(
                after_serial=cursor, limit=_DRAIN_PAGE_LIMIT
            )
            if not tokens:
                return attempts
            for token in tokens:
                cursor = token.serial
                client_conn = self._connection_for_serial(token.serial)
                if client_conn is None:
                    continue
                # A scheduled close is not a confirmed close: retry every
                # expired transport that is still registered until the
                # client_disconnected hook releases it.
                if self._close_established(client_conn):
                    attempts += 1
            if len(tokens) < _DRAIN_PAGE_LIMIT:
                return attempts

    def _close_established(self, client_conn: Any) -> bool:
        """Force-close one established transport from the drain thread.

        mitmproxy owns the socket inside its event loop; the supported path is
        the proxyserver connection registry, reached through a thread-safe
        loop call. Without a captured loop (fake contexts, pre-loop state) this
        falls back to the connection-state fence, which tears the transport
        down on its next event.
        """
        try:
            loop, proxyserver = self._loop, self._proxyserver
            if loop is not None and proxyserver is not None:
                handler = proxyserver.connections.get(client_conn.id)
                if handler is not None:
                    # Scheduling is not a confirmed close: keep the transport
                    # retryable on the next sweep until client_disconnected
                    # releases the registry admission.
                    loop.call_soon_threadsafe(
                        _invoke_handler_close, handler, client_conn
                    )
                    return True
        except Exception:  # noqa: BLE001 - never raise into the drain loop
            pass
        return close_connection(client_conn)

    def _connection_for_serial(self, serial: int) -> Any:
        with self._lock:
            key = self._serials.get(serial)
            if key is None:
                return None
            entry = self._connections.get(key)
            return None if entry is None else entry[1]

    # -- Shutdown ----------------------------------------------------------

    def close(self) -> None:
        """Stop the drain thread and fence new admissions.

        The IPC server's close() fences the receiver; system.py owns it.
        """
        self._drain_stop.set()
        if self._drain_thread.is_alive() and threading.current_thread() is not self._drain_thread:
            self._drain_thread.join(timeout=2)
        with self._lock:
            self._closed = True


def _invoke_handler_close(handler: Any, client_conn: Any) -> None:
    """Run inside the proxy event loop; never raise into it."""
    try:
        handler.close_connection(client_conn)
    except Exception:  # noqa: BLE001 - transport errors are not actionable
        pass


def _mark_closed(client_conn: Any) -> None:
    """Local bookkeeping marker; mitmproxy state objects accept attributes."""
    try:
        client_conn.closed = True
    except Exception:  # noqa: BLE001 - defensive only
        pass


def close_connection(client_conn: Any) -> bool:
    """Fence one transport closed by state, on or off the event loop.

    The mitmproxy Client object is live shared state: marking it CLOSED makes
    the TLS layers treat the transport as already gone (verified against
    mitmproxy 11.0.2: a ClientHello-time CLOSE state yields an immediate
    client EOF) and the handler tears the socket down on its next event.
    """
    try:
        from mitmproxy.connection import ConnectionState

        state: Any = ConnectionState.CLOSED
    except Exception:  # noqa: BLE001 - fake contexts have no real module
        state = "closed"
    try:
        client_conn.state = state
        _mark_closed(client_conn)
        return True
    except Exception:  # noqa: BLE001 - never raise into mitmproxy hooks
        return False


def terminate_handshake(client_conn: Any) -> bool:
    """Deny one connection during its TLS handshake (in-loop, in-hook).

    Equivalent to close_connection: the ClientHello layer checks connection
    liveness right after the tls_clienthello hook, so a CLOSED state ends the
    handshake with a client-visible EOF instead of decryption or tunneling.
    """
    return close_connection(client_conn)


_runtime: LiveRuntime | None = None


def enabled() -> bool:
    """True when the live credential-bound runtime owns this process."""
    return _runtime is not None


def configure(
    socket_path: str,
    token: str,
    control: str,
    subject: str,
    limit: int,
) -> tuple[LiveRuntime, Any]:
    """Build the live runtime and its IPC server from the launcher bundle.

    Called by system.py load() after the base revision bundle validated. Any
    invalid live configuration exits the process like a partial base bundle.
    Returns the runtime and the started IPC server; the caller owns closing
    the server (which fences the receiver) during addon shutdown.
    """
    global _runtime
    if _runtime is not None:
        _fatal()
    live = _live_configuration()
    if live is None:
        _fatal()
    tls_capacity, request_capacity, drain_timeout = live
    from revision_ipc import Server

    receiver = LiveReceiver(
        control,
        subject,
        max_snapshot_bytes=limit,
        capacity=tls_capacity,
        request_capacity=request_capacity,
        drain_timeout_seconds=drain_timeout,
    )
    server = Server(
        receiver,
        socket_path,
        token,
        max_snapshot_bytes=limit,
        request_timeout=1,
    )
    server.start()
    runtime = LiveRuntime(receiver, identity=(control, subject))
    runtime.start()
    _runtime = runtime
    return runtime, server


def reset_for_tests() -> None:
    """Test-only: detach the module-level runtime without IPC cleanup."""
    global _runtime
    _runtime = None


def runtime() -> LiveRuntime | None:
    return _runtime
