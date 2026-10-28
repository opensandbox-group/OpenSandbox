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
- A TLS decision uses frozen static selectors and the installed immutable
  snapshot. Unknown identities deny; bootstrapping permits only validated
  static TLS/443 and the existing ECH/no-SNI opaque decisions.
- A decrypted connection exists only with a registry admission; a request on a
  connection without one is rejected, never silently forwarded with credentials.
- Removed hosts fence new requests on tracked connections immediately; the
  bounded retirement deadline force-closes the transport at expiry. No fence is
  reversible, so remove/re-add cannot resurrect a retired connection.
"""

from __future__ import annotations

import asyncio
import os
import threading
from contextlib import suppress
from dataclasses import dataclass
from typing import Any

from mitmproxy import ctx
from host_selectors import Selector
from revision_publication import LiveReceiver
from static_passthrough import ENV as PASSTHROUGH_ENV, ERROR as PASSTHROUGH_ERROR, NATIVE_OPTIONS_ERROR, parse_handoff
from tls_registry import (
    AdmissionResult,
    AdmissionToken,
    RequestAdmission,
    RequestHandle,
)

# Fixed-vocabulary diagnostics; never a host, revision, subject or secret.
_DRAIN_INTERVAL_SECONDS = 0.5
_DRAIN_PAGE_LIMIT = 64
_LIVE_RUNTIME_ERROR = "credential proxy: invalid revision runtime configuration"
_H2_PROCESS_EXIT_CODE = 86
_H2_OWNERSHIP_EXIT_CODE = 87
_ABSENT = object()
_H2_OWNERSHIP_ERROR = "credential proxy: unverified H2 transport ownership"

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


def _exact_entry(mapping: Any, key: Any, value: Any) -> bool:
    return any(k is key and v is value for k, v in mapping.items())


def _layer_chain(root: Any) -> Any:
    """Read the pinned dependency's actual ancestry, without changing layers."""
    from mitmproxy.proxy import layer

    seen: set[int] = set()
    while root is not None:
        if id(root) in seen:
            raise ValueError(_H2_OWNERSHIP_ERROR)
        seen.add(id(root))
        yield root
        root = root.layer if isinstance(root, layer.NextLayer) else getattr(root, "child_layer", None)


@dataclass(eq=False)
class _H2Transport:
    """One current upstream task, retained only while cleanup needs its proof."""

    adapter: Any
    root: Any
    handler: Any
    client: Any
    connection: Any
    owner: Any
    task: asyncio.Task
    token: AdmissionToken
    connected_io: Any = None
    connect_error_io: Any = None
    close_io: Any = None
    disconnected_hook_seen: bool = False
    close_consumed: bool = False
    terminal: bool = False


class LiveRuntime:
    """Transport bookkeeping for one live credential-bound mitmdump process.

    One instance serves exactly one sidecar generation; a replacement process
    builds a fresh receiver, registry and runtime.
    """

    def __init__(self, receiver: LiveReceiver, identity: tuple[str, str]) -> None:
        self._receiver = receiver
        self.registry = receiver.registry
        self.identity = identity
        self._static_passthrough = receiver.static_passthrough
        self._lock = threading.Lock()
        # id(client_conn) -> (token, client_conn). Holding the connection
        # reference keeps the id unique while tracked.
        self._connections: dict[int, tuple[AdmissionToken, Any]] = {}
        self._serials: dict[int, int] = {}
        # H2 protocol state belongs to one in-loop adapter per tracked client.
        # Transport observations are current tasks, never a request history.
        self._h2: dict[int, tuple[Any, Any]] = {}
        self._h2_transports: dict[asyncio.Task, _H2Transport] = {}
        self._h2_disconnected: set[int] = set()
        self._h2_wakeup_hooks: dict[int, tuple[Any, Any, Any, set]] = {}
        self._h2_exit_started = False
        # Captured inside the event loop at the first tracked handshake; used
        # by the drain thread to schedule thread-safe transport closure.
        self._loop: Any = None
        self._proxyserver: Any = None
        self._drain_stop = threading.Event()
        self._drain_thread = threading.Thread(
            target=self._drain_loop, name="credential-bound-drain", daemon=True
        )
        self._closed = False

    @property
    def static_passthrough(self) -> tuple[Selector, ...]:
        return self._static_passthrough

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

    def admit_tls(self, sni: str | None, ech_hidden: bool = False,
                  destination_port: int | None = None) -> AdmissionResult:
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
            static_passthrough=self.static_passthrough,
            destination_port=destination_port,
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
            return None if entry is None or entry[1] is not client_conn else entry[0]

    def h2_for(self, client_conn: Any) -> Any:
        with self._lock:
            entry = self._h2.get(id(client_conn))
            return None if entry is None or entry[0] is not client_conn else entry[1]

    def attach_h2(self, client_conn: Any, adapter: Any) -> bool:
        """Attach an unstarted instance to the same tracked TLS admission."""
        from mitmproxy.proxy.layers.http import HttpLayer
        from mitmproxy.proxy.layers.http._http2 import Http2Server

        if (
            not self._on_h2_loop()
            or not isinstance(adapter, Http2Server)
            or adapter.runtime is not self
            or not isinstance(adapter.http_layer, HttpLayer)
            or adapter.context.client is not client_conn
            or adapter.http_layer.context.client is not client_conn
            or not _exact_entry(adapter.http_layer.connections, client_conn, adapter)
            or self.token_for(client_conn) is not adapter.token
            or self._connection_for_serial(adapter.token.serial) is not client_conn
        ):
            raise ValueError(_H2_OWNERSHIP_ERROR)
        with self._lock:
            if self._closed or id(client_conn) in self._h2_disconnected:
                raise ValueError(_H2_OWNERSHIP_ERROR)
            previous = self._h2.get(id(client_conn))
            if previous is not None and previous != (client_conn, adapter):
                raise ValueError(_H2_OWNERSHIP_ERROR)
            self._h2[id(client_conn)] = (client_conn, adapter)
        return True

    def _h2_handler(self, adapter: Any) -> Any:
        from mitmproxy.proxy import server

        client, root = adapter.context.client, adapter.http_layer
        handler = getattr(self._proxyserver, "connections", {}).get(client.id)
        if not (
            self._on_h2_loop() and not self._closed
            and self.h2_for(client) is adapter
            and self.token_for(client) is adapter.token
            and self._connection_for_serial(adapter.token.serial) is client
            and isinstance(handler, server.ConnectionHandler)
            and handler.client is client and root.context.client is client
            and any(node is root for node in _layer_chain(handler.layer))
            and _exact_entry(root.connections, client, adapter)
        ):
            raise ValueError(_H2_OWNERSHIP_ERROR)
        return handler

    def filter_h2_wakeups(self, adapter: Any) -> None:
        """Filter only this adapter's ordinary, dependency-registered wakeup.

        Poll in the existing wakeup task without delivering no-op server events:
        those events reset the dependency watchdog even without network traffic.
        Neither command routing maps nor dependency classes are patched.
        """
        handler = self._h2_handler(adapter)
        key = id(adapter.context.client)
        installed = self._h2_wakeup_hooks.get(key)
        if installed is not None:
            if installed[0] is not handler or handler.wakeup is not installed[2]:
                raise ValueError(_H2_OWNERSHIP_ERROR)
            return
        from mitmproxy.proxy import server

        previous = handler.__dict__.get("wakeup", _ABSENT)
        original = handler.wakeup
        if (getattr(original, "__self__", None) is not handler
                or getattr(original, "__func__", None) is not server.ConnectionHandler.wakeup):
            raise ValueError(_H2_OWNERSHIP_ERROR)
        tasks: set[asyncio.Task] = set()

        async def wakeup(request):
            from mitmproxy.proxy import events

            if request is not adapter._wakeup:
                await original(request)
                return
            task = asyncio.current_task()
            tasks.add(task)
            try:
                while True:
                    # Preserve the dependency's initial suspension and its
                    # registration/cancellation ordering, even for fast tasks.
                    await asyncio.sleep(request.delay)
                    if adapter.client_closed or adapter.close_started:
                        return
                    if (request is not adapter._wakeup
                            or handler.wakeup is not wakeup
                            or self._h2_handler(adapter) is not handler):
                        raise ValueError(_H2_OWNERSHIP_ERROR)
                    if adapter.maintenance_needed():
                        handler.wakeup_timer.discard(task)
                        await handler.server_event(events.Wakeup(request))
                        return
            except asyncio.CancelledError:
                raise
            except Exception:  # noqa: BLE001 - never silently lose drain checks
                self._block_h2_cleanup(adapter)
            finally:
                tasks.discard(task)
                handler.wakeup_timer.discard(task)

        handler.wakeup = wakeup
        self._h2_wakeup_hooks[key] = (handler, previous, wakeup, tasks)

    def _restore_h2_wakeups(self, client: Any) -> None:
        installed = self._h2_wakeup_hooks.pop(id(client), None)
        if installed is None:
            return
        handler, previous, wrapper, tasks = installed
        if handler.wakeup is wrapper:
            if previous is _ABSENT:
                del handler.wakeup
            else:
                handler.wakeup = previous
        for task in tuple(tasks):
            task.cancel()

    # -- Request admission path -------------------------------------------

    def acquire_request(
        self, token: AdmissionToken | None, *, client_stream_id: int | None = None
    ) -> Any:
        """Delegate to the registry; see BoundConnectionRegistry.acquire_request."""
        serial = getattr(token, "serial", None)
        client = self._connection_for_serial(serial) if type(serial) is int else None
        adapter = self.h2_for(client)
        if adapter is not None and (
            adapter.client_closed or id(client) in self._h2_disconnected
        ):
            return RequestAdmission("deny", "connection_fenced")
        return self.registry.acquire_request(token, client_stream_id=client_stream_id)

    def finish_request(self, handle: RequestHandle | None) -> None:
        """Idempotent terminal cleanup for one admitted request."""
        self.registry.finish_request(handle)

    # -- Connection lifecycle ----------------------------------------------

    def client_disconnected(self, client_conn: Any) -> None:
        """Fence immediately; H2 keeps ownership until upstream cleanup is proven."""
        if client_conn is None:
            return
        adapter = self.h2_for(client_conn)
        if adapter is not None:
            self._restore_h2_wakeups(client_conn)
            self._h2_disconnected.add(id(client_conn))
            adapter.on_client_disconnected()
            try:
                if not self._on_h2_loop():
                    raise ValueError(_H2_OWNERSHIP_ERROR)
                # A completed request may have just released its handle while
                # its idle transport is still live. Include observed tasks too.
                for conn, owner in adapter.owned_upstreams():
                    self._observe_h2(adapter, conn, owner)
                for item in tuple(self._h2_transports.values()):
                    if item.adapter is not adapter or item.terminal:
                        continue
                    if not item.close_consumed:
                        self._close_h2_in_hook(item)
                    elif item.task.done():
                        self._h2_task_done(item, item.task)
                self._release_h2_if_terminal(adapter)
            except Exception:  # noqa: BLE001 - unknown cleanup is never success
                self._block_h2_cleanup(adapter)
            return
        self._release_client(client_conn)

    def _release_client(self, client_conn: Any) -> None:
        with self._lock:
            entry = self._connections.get(id(client_conn))
            if entry is not None and entry[1] is client_conn:
                self._connections.pop(id(client_conn))
            else:
                entry = None
            if entry is not None:
                self._serials.pop(entry[0].serial, None)
        if entry is not None:
            self.registry.release(entry[0])

    def _on_h2_loop(self) -> bool:
        try:
            return self._loop is not None and asyncio.get_running_loop() is self._loop
        except RuntimeError:
            return False

    def _h2_ownership_current(self, item: _H2Transport) -> bool:
        """D3 is tied to the currently registered handler and actual child graph."""
        from mitmproxy.connection import Server
        from mitmproxy.proxy import server
        from mitmproxy.proxy.layers.http import HttpClient

        try:
            root = item.root
            return (
                self._on_h2_loop()
                and self.token_for(item.client) is item.token
                and self._connection_for_serial(item.token.serial) is item.client
                and self.h2_for(item.client) is item.adapter
                and item.adapter.runtime is self
                and item.adapter.http_layer is root
                and item.adapter.token is item.token
                and self._proxyserver.connections.get(item.client.id) is item.handler
                and isinstance(item.handler, server.ConnectionHandler)
                and item.handler.client is item.client
                and root.context.client is item.client
                and item.adapter.context.client is item.client
                and any(node is root for node in _layer_chain(item.handler.layer))
                and _exact_entry(root.connections, item.client, item.adapter)
                and isinstance(item.connection, Server)
                and item.connection.transport_protocol == "tcp"
                and _exact_entry(root.connections, item.connection, item.owner)
                and all(node.context.client is item.client for node in _layer_chain(item.owner))
                and sum(
                    isinstance(node, HttpClient) and node.context.server is item.connection
                    for node in _layer_chain(item.owner)
                ) == 1
                and item.task.get_loop() is self._loop
            )
        except Exception:  # noqa: BLE001 - stale or unsupported ancestry
            return False

    def _observe_h2(self, adapter: Any, conn: Any, owner: Any) -> _H2Transport:
        from mitmproxy.proxy import server

        if not self._on_h2_loop():
            raise ValueError(_H2_OWNERSHIP_ERROR)
        client = adapter.context.client
        handler = self._proxyserver.connections.get(client.id)
        if not isinstance(handler, server.ConnectionHandler):
            raise ValueError(_H2_OWNERSHIP_ERROR)  # noqa: TRY004 - unavailable owner
        # A verified terminal task can remain only for current active handles.
        for item in self._h2_transports.values():
            if item.adapter is adapter and item.connection is conn and item.owner is owner:
                if not self._h2_ownership_current(item):
                    raise ValueError(_H2_OWNERSHIP_ERROR)
                current = handler.transports.get(conn)
                if current is not None and (
                    not _exact_entry(handler.transports, conn, current)
                    or current.handler is not item.task
                ):
                    raise ValueError(_H2_OWNERSHIP_ERROR)
                return item
        current = handler.transports.get(conn)
        if (
            current is None
            or not _exact_entry(handler.transports, conn, current)
            or not isinstance(current.handler, asyncio.Task)
        ):
            raise ValueError(_H2_OWNERSHIP_ERROR)
        item = _H2Transport(
            adapter=adapter, root=adapter.http_layer, handler=handler, client=client,
            connection=conn, owner=owner, task=current.handler,
            token=adapter.token,
        )
        if not self._h2_ownership_current(item):
            raise ValueError(_H2_OWNERSHIP_ERROR)
        if item.task in self._h2_transports:
            raise ValueError(_H2_OWNERSHIP_ERROR)
        self._h2_transports[item.task] = item
        item.task.add_done_callback(lambda completed: self._h2_task_done(item, completed))
        return item

    def prepare_h2_close(self, adapter: Any, conn: Any, owner: Any) -> _H2Transport:
        """Capture the exact upstream task before yielding normal CloseConnection."""
        item = self._observe_h2(adapter, conn, owner)
        current = item.handler.transports.get(conn)
        if current is None or current.handler is not item.task:
            raise ValueError(_H2_OWNERSHIP_ERROR)
        item.close_io = current
        return item

    def h2_close_consumed(self, item: _H2Transport) -> None:
        """Called only after the dependency resumes a yielded close command."""
        if (
            self._h2_transports.get(item.task) is not item
            or not self._h2_ownership_current(item)
            or item.close_io is None
        ):
            raise ValueError(_H2_OWNERSHIP_ERROR)
        item.close_consumed = True
        if item.task.done():
            self._h2_task_done(item, item.task)

    def _close_h2_in_hook(self, item: _H2Transport) -> None:
        # ClientDisconnectedHook cannot yield protocol commands. This is the
        # dispatcher's same synchronous entry; an exception is NOT consumption.
        self.prepare_h2_close(item.adapter, item.connection, item.owner)
        item.handler.close_connection(item.connection, False)
        self.h2_close_consumed(item)

    def _server_h2_hook(self, data: Any, phase: str) -> None:
        adapter = self.h2_for(data.client)
        if adapter is None:
            return
        try:
            owner = next(
                owner for conn, owner in adapter.http_layer.connections.items()
                if conn is data.server
            )
            item = self._observe_h2(adapter, data.server, owner)
            if asyncio.current_task() is not item.task:
                raise ValueError(_H2_OWNERSHIP_ERROR)
            if phase == "connect":
                # Cache each active request's exact pending command association
                # before fast connect failure removes that command. The stale
                # transparent flow.server_conn is not a replacement identity.
                if not any(
                    conn is item.connection and candidate is item.owner
                    for conn, candidate in adapter.owned_upstreams()
                ):
                    raise ValueError(_H2_OWNERSHIP_ERROR)
            elif phase == "connected":
                current = item.handler.transports.get(item.connection)
                if current is None or current.handler is not item.task:
                    raise ValueError(_H2_OWNERSHIP_ERROR)
                item.connected_io = current
            elif phase == "connect_error":
                current = item.handler.transports.get(item.connection)
                if (
                    current is None or current.handler is not item.task
                    or current.writer is not None
                    or item.connection.timestamp_tcp_setup is not None
                    or not item.connection.error
                ):
                    raise ValueError(_H2_OWNERSHIP_ERROR)
                item.connect_error_io = current
            elif phase == "disconnected":
                item.disconnected_hook_seen = True
        except Exception:  # noqa: BLE001 - unknown cleanup is retained and fenced
            self._block_h2_cleanup(adapter)

    def server_connect(self, data: Any) -> None:
        self._server_h2_hook(data, "connect")

    def server_connected(self, data: Any) -> None:
        self._server_h2_hook(data, "connected")

    def server_connect_error(self, data: Any) -> None:
        self._server_h2_hook(data, "connect_error")

    def server_disconnected(self, data: Any) -> None:
        self._server_h2_hook(data, "disconnected")

    def _h2_task_done(self, item: _H2Transport, completed: Any) -> None:
        """Read current IO on the owning loop; never run a protocol generator."""
        from mitmproxy.connection import ConnectionState

        if (
            self._h2_exit_started
            or self._h2_transports.get(item.task) is not item
            or completed is not item.task
            or not item.task.done()
            or not self._on_h2_loop()
        ):
            return
        # Released/replaced admissions make this callback stale. An admission
        # still owned by this runtime cannot silently retain capacity when its
        # completed task's physical ownership proof has become unverifiable.
        if (self.h2_for(item.client) is not item.adapter
                or self.token_for(item.client) is not item.token
                or self._connection_for_serial(item.token.serial) is not item.client):
            return
        if not self._h2_ownership_current(item):
            self._block_h2_cleanup(item.adapter)
            return
        current = item.handler.transports.get(item.connection)
        if current is not None and (
            not _exact_entry(item.handler.transports, item.connection, current)
            or current.handler is not item.task
        ):
            self._block_h2_cleanup(item.adapter)
            return
        if current is None:
            io = item.connected_io or item.close_io
            item.terminal = bool(
                item.disconnected_hook_seen
                and io is not None
                and isinstance(io.writer, asyncio.StreamWriter)
                and io.writer.is_closing()
                and item.connection.state is ConnectionState.CLOSED
            )
        elif current.handler is item.task and current.writer is None:
            cancelled_before_connect = item.close_consumed and item.task.cancelled()
            failed_before_connect = (
                item.connect_error_io is current
                and bool(item.connection.error)
                and not item.task.cancelled()
                and item.task.exception() is None
            )
            item.terminal = bool(
                (cancelled_before_connect or failed_before_connect)
                and item.connection.timestamp_tcp_setup is None
                and item.connection.state is ConnectionState.CLOSED
            )
        elif (
            item.close_consumed
            and current.handler is item.task
            and isinstance(current.writer, asyncio.StreamWriter)
            and not current.writer.is_closing()
            and item.connection.state is ConnectionState.CLOSED
        ):
            # D3: flag first, then hard-exit this mitmdump only. Nothing that
            # could block or fail (logging, IPC, cleanup) may precede the exit.
            self._h2_exit_started = True
            os._exit(_H2_PROCESS_EXIT_CODE)
            return
        if item.terminal:
            self.prune_h2_transports(item.adapter)
            self._release_h2_if_terminal(item.adapter)
        else:
            self._block_h2_cleanup(item.adapter)

    def h2_transport_known_terminal(self, adapter: Any, conn: Any, owner: Any) -> bool:
        """Read existing terminal proof without observing tasks or releasing state."""
        return any(
            item.adapter is adapter and item.connection is conn and item.owner is owner
            and item.terminal and self._h2_ownership_current(item)
            for item in self._h2_transports.values()
        )

    def h2_transport_terminal(self, adapter: Any, conn: Any, owner: Any) -> bool:
        try:
            item = self._observe_h2(adapter, conn, owner)
        except ValueError:
            return False
        if item.task.done():
            self._h2_task_done(item, item.task)
        return item.terminal

    def prune_h2_transports(self, adapter: Any) -> None:
        """Drop terminal proof once no active request needs it; retain live IO."""
        if not self._on_h2_loop():
            raise ValueError(_H2_OWNERSHIP_ERROR)
        try:
            active = tuple(adapter.owned_upstreams())
        except Exception:  # noqa: BLE001
            self._block_h2_cleanup(adapter)
            return
        for task, item in tuple(self._h2_transports.items()):
            if item.adapter is adapter and item.terminal and not any(
                conn is item.connection and owner is item.owner for conn, owner in active
            ):
                self._h2_transports.pop(task)

    def _release_h2_if_terminal(self, adapter: Any) -> None:
        client = adapter.context.client
        if (
            id(client) not in self._h2_disconnected
            or self._h2_exit_started
            or self.h2_for(client) is not adapter
            or self.token_for(client) is not adapter.token
        ):
            return
        if any(item.adapter is adapter and not item.terminal for item in self._h2_transports.values()):
            return
        # Each still-associated active upstream must have its own terminal proof.
        try:
            for conn, owner in adapter.owned_upstreams():
                if not any(
                    item.adapter is adapter and item.connection is conn
                    and item.owner is owner and item.terminal
                    for item in self._h2_transports.values()
                ):
                    return
        except Exception:  # noqa: BLE001
            self._block_h2_cleanup(adapter)
            return
        for task, item in tuple(self._h2_transports.items()):
            if item.adapter is adapter:
                self._h2_transports.pop(task)
        with self._lock:
            self._h2.pop(id(client), None)
        self._h2_disconnected.discard(id(client))
        self._release_client(client)

    def _block_h2_cleanup(self, adapter: Any) -> None:
        # Unverifiable cleanup is a distinct fatal recovery condition, not D3's
        # proven current-writer defect. Do not release unproven resources or
        # leave a healthy-looking process holding their budgets indefinitely.
        if self._h2_exit_started:
            return
        self._h2_exit_started = True
        os._exit(_H2_OWNERSHIP_EXIT_CODE)

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
                if client_conn is None or self.h2_for(client_conn) is not None:
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
        for client, _ in tuple(self._h2.values()):
            self._restore_h2_wakeups(client)
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


def validate_live_options(options: Any | None = None) -> None:
    """Native pre-TLS selectors must never bypass live admission."""
    if options is None:
        options = ctx.options
    if any(getattr(options, name, ()) for name in ("ignore_hosts", "allow_hosts")):
        raise SystemExit(NATIVE_OPTIONS_ERROR) from None
    if getattr(options, "ssl_insecure", False):
        _fatal()


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
    validate_live_options()
    try:
        static_passthrough = parse_handoff(os.environ.get(PASSTHROUGH_ENV))
    except ValueError:
        raise SystemExit(PASSTHROUGH_ERROR) from None
    if static_passthrough and os.environ.get("OPENSANDBOX_EGRESS_UPSTREAM_PROXY", "").strip():
        _fatal()
    from revision_ipc import Server

    receiver = LiveReceiver(
        control,
        subject,
        max_snapshot_bytes=limit,
        capacity=tls_capacity,
        request_capacity=request_capacity,
        drain_timeout_seconds=drain_timeout,
        static_passthrough=static_passthrough,
    )
    server = Server(
        receiver,
        socket_path,
        token,
        max_snapshot_bytes=limit,
        request_timeout=1,
    )
    runtime = LiveRuntime(receiver, identity=(control, subject))
    server.start()
    runtime.start()
    _runtime = runtime
    return runtime, server


def reset_for_tests() -> None:
    """Test-only: detach the module-level runtime without IPC cleanup."""
    global _runtime
    _runtime = None


def runtime() -> LiveRuntime | None:
    return _runtime
