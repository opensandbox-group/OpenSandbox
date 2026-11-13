# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Experimental subject-bound TLS and HTTP/1.1 request admission.

Only dispatch snapshots feed selector publication. No receiver readback/acquire,
IPC, or owner operation lock is reachable from a ClientHello. A binding
transition fences new requests at commit and the drain loop closes decrypted
transports whose retirement deadline expired. B4 opaque teardown (Unload,
suspend, rebind) is still missing: this gate must not be enabled in production
before B4.
"""

from __future__ import annotations

import asyncio
import threading
from contextlib import suppress

from static_passthrough import StaticPassthrough
from tls_decision import _classify, compile_view
from tls_registry import AdmissionResult, BoundConnectionRegistry, RequestAdmission

# Same cadence as the sidecar drain thread; the page size is the registry maximum.
_DRAIN_INTERVAL_SECONDS = 0.5
_DRAIN_PAGE_LIMIT = 128


class SubjectRegistry(BoundConnectionRegistry):
    """One entry lifetime; invalidation never releases a live transport token."""

    def __init__(self, *, capacity, accounting, static_passthrough, drain_timeout_seconds=30):
        super().__init__(capacity=capacity, accounting=accounting,
                         drain_timeout_seconds=drain_timeout_seconds)
        self._static_config = StaticPassthrough(static_passthrough)
        self._dispatch_revision = None
        self._dispatch_snapshot = None

    @property
    def static_selectors(self):
        # admit() skips revalidation only for this exact startup-owned tuple;
        # an equal but distinct tuple must still take the validation path.
        return self._static_config.selectors

    def publish_dispatch(self, snapshot):
        revision = None if snapshot is None else snapshot.revision
        with self._lock:
            if self._closed or revision == self._dispatch_revision:
                return
        # Compilation is on the install writer, before the short registry lock.
        view = None if snapshot is None else compile_view(snapshot)
        with self._lock:
            # Compilation ran outside the lock; another writer or shutdown may
            # have published in the meantime. Keep the publication-time check.
            if self._closed or revision == self._dispatch_revision:
                return
            if view is None:
                # Suspension revokes every existing membership permanently.
                # Activation preserves this fence; only disconnect frees slots.
                self._request_fenced = self._request_fenced.union(self._entries)
                # Clearing the view removes the registry's epoch comparison
                # on reinstall; receiver prepare/commit validation remains
                # authoritative. The membership fence survives any new view.
                self._view = None
            else:
                self._publish_activation(self._plan_activation(view))
            self._dispatch_revision = revision
            self._dispatch_snapshot = snapshot

    def deactivate(self):
        # Clear the dispatch payload in the same critical section as shutdown.
        # External request handles keep their own pinned snapshots alive.
        with self._lock:
            self._check_standalone()
            tokens = tuple(self._entries.values())
            self._closed = True
            self._view = None
            self._dispatch_revision = None
            self._dispatch_snapshot = None
            return tokens

    def _request_snapshot_missing_locked(self):
        return self._dispatch_snapshot is None

    def _request_snapshot_locked(self):
        # Both snapshot and selector revision are published under this lock.
        # Never consult receiver installation state or source-IP pull caches.
        return self._dispatch_snapshot

    def budget_observations(self):
        with self._lock, self._accounting._lock:
            return (("process", self._accounting.occupied, self._accounting.capacity),
                    ("subject", len(self._entries), self._capacity))


def admit_source(owner, address, sni, *, ech_hidden=False, destination_port=None):
    dispatch = owner.lookup_source(address)
    registry = None if dispatch is None else dispatch.registry
    if registry is None:
        result = _classify(identity=None, sni=sni, ech_hidden=ech_hidden,
                           static_passthrough=(), view=None, destination_port=destination_port)
        return AdmissionResult(result.action, result.reason), None
    return registry.admit(
        identity=(owner.control_generation, dispatch.target.subject_generation),
        sni=sni, ech_hidden=ech_hidden,
        static_passthrough=registry.static_selectors,
        destination_port=destination_port,
    ), registry


class SubjectAdmissionRuntime:
    """Connection ownership stays pinned to the handshake registry across IP reuse.

    Requests consume these exact (registry, token, client) memberships. No
    registry slot is released on Unload or suspend; client_disconnected is
    the terminal signal. The drain loop only requests closure of transports
    whose registry retirement deadline expired; Unload/suspend teardown
    remains follow-up work.
    """

    def __init__(self, owner):
        self.owner = owner
        self._lock = threading.Lock()
        self._connections = {}
        self._tokens = {}
        # (event loop, proxyserver addon), captured inside a TLS hook; the
        # drain thread only schedules closure onto that loop.
        self._transport = (None, None)
        self._drain_stop = threading.Event()
        self._drain_thread = None

    def admit_tls(self, client, sni, *, ech_hidden=False, destination_port=None):
        try:
            address = client.peername[0]
        except (AttributeError, IndexError, TypeError):
            address = ""
        result, registry = admit_source(self.owner, address, sni,
                                       ech_hidden=ech_hidden, destination_port=destination_port)
        if result.token is not None:
            self._capture_transport()
            with self._lock:
                previous = self._connections.get(id(client))
                # No renegotiation may replace or prematurely release a live token.
                if previous is None:
                    item = (registry, result.token, client)
                    self._connections[id(client)] = item
                    self._tokens[id(result.token)] = item
            if previous is not None:
                registry.release(result.token)
                return AdmissionResult("deny", "invalid_input")
        if registry is not None:
            self._observe(registry)
        return result

    def token_for(self, client):
        with self._lock:
            item = self._connections.get(id(client))
            return None if item is None or item[2] is not client else item[1]

    def h2_for(self, client):
        return None

    def acquire_request(self, token):
        with self._lock:
            item = self._tokens.get(id(token))
            if item is None or item[1] is not token:
                return RequestAdmission("deny", "invalid_token")
            return item[0].acquire_request(token)

    def finish_request(self, handle):
        if handle is None:
            return
        with self._lock:
            item = self._tokens.get(id(handle.connection))
            if item is not None and item[1] is handle.connection:
                item[0].finish_request(handle)

    @staticmethod
    def _observe(registry):
        # Fixed scope vocabulary; no identity/SNI/revision in metrics labels.
        with suppress(Exception):
            from mitmproxy import ctx
            for scope, occupied, capacity in registry.budget_observations():
                ctx.log.warn(f"credential proxy: tls-budget-v1 {scope} {occupied} {capacity}")

    def client_disconnected(self, client):
        with self._lock:
            item = self._connections.get(id(client))
            if item is None or item[2] is not client:
                return
            del self._connections[id(client)]
            del self._tokens[id(item[1])]
        registry, token, _ = item
        registry.release(token)
        self._observe(registry)

    # -- Drain ---------------------------------------------------------------

    def _capture_transport(self):
        """Record the proxy loop and connection registry; in-loop hooks only."""
        if self._transport[0] is not None:
            return
        try:
            loop = asyncio.get_running_loop()
        except RuntimeError:
            return
        try:
            from mitmproxy import ctx
            proxyserver = ctx.master.addons.get("proxyserver")
        except Exception:  # noqa: BLE001 - no master outside a proxy process
            proxyserver = None
        if proxyserver is not None:
            self._transport = (loop, proxyserver)

    def start(self):
        """Start the single process-level drain loop of this runtime."""
        with self._lock:
            if self._drain_thread is not None or self._drain_stop.is_set():
                raise RuntimeError("subject drain loop already started")
            self._drain_thread = threading.Thread(
                target=self._drain_loop, name="subject-admission-drain", daemon=True)
            thread = self._drain_thread
        thread.start()

    def close(self):
        """Stop the drain loop. Memberships stay until client_disconnected."""
        self._drain_stop.set()
        thread = self._drain_thread
        if (thread is not None and thread.is_alive()
                and thread is not threading.current_thread()):
            thread.join(timeout=2)

    def _drain_loop(self):
        while not self._drain_stop.wait(_DRAIN_INTERVAL_SECONDS):
            try:
                self.drain_sweep_once()
            except Exception:  # noqa: BLE001 - diagnostics must never kill the loop
                with suppress(Exception):
                    from mitmproxy import ctx
                    ctx.log.warn("credential proxy: subject drain error")

    def drain_sweep_once(self):
        """Request closure of every held transport whose retirement expired.

        Only registries that still hold a runtime membership are inspected,
        including registries already removed from the owner index. Each sweep
        restarts every registry at serial zero and pages to its end, so a
        failed close neither stalls later pages nor is forgotten: the next
        sweep retries it. Inspection never refreshes a deadline. A scheduled
        close is not a release; only client_disconnected frees the budget.
        """
        with self._lock:
            registries = {id(item[0]): item[0] for item in self._connections.values()}
        attempts = 0
        for registry in registries.values():
            cursor = 0
            while True:
                tokens = registry.expired_connections(after_serial=cursor, limit=_DRAIN_PAGE_LIMIT)
                for token in tokens:
                    cursor = token.serial
                    client = self._member_client(registry, token)
                    if client is not None and self._request_close(client):
                        attempts += 1
                if len(tokens) < _DRAIN_PAGE_LIMIT:
                    break
        return attempts

    def _member_client(self, registry, token):
        # Serials repeat across registries: match the exact token and registry.
        with self._lock:
            item = self._tokens.get(id(token))
            if item is None or item[0] is not registry or item[1] is not token:
                return None
            return item[2]

    def _request_close(self, client):
        """Schedule closure on the proxy loop; never holds a runtime lock."""
        loop, proxyserver = self._transport
        if loop is None:
            return False
        try:
            loop.call_soon_threadsafe(_close_in_loop, proxyserver, client)
        except Exception:  # noqa: BLE001 - a closed loop is retried next sweep
            return False
        return True


def _close_in_loop(proxyserver, client):
    """Close one client transport inside the proxy loop; never raise into it."""
    with suppress(Exception):
        handler = proxyserver.connections.get(client.id)
        if handler is not None:
            handler.close_connection(client)


_runtime = None


def runtime():
    return _runtime
