# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Experimental subject-bound TLS and HTTP/1.1 request admission.

Only dispatch snapshots feed selector publication. No receiver readback/acquire,
IPC, or owner operation lock is reachable from a ClientHello. A binding
transition fences new requests at commit and the drain loop closes decrypted
transports whose retirement deadline expired. Lifecycle teardown (Unload,
suspend, receiver failure, owner terminal/close and source reuse before
Register) closes every proxied client of the old identity, decrypted or
opaque. Traffic that never reaches mitmproxy is outside this index.
"""

from __future__ import annotations

import asyncio
import threading
import time
from contextlib import suppress
from dataclasses import dataclass, field

from static_passthrough import StaticPassthrough
from tls_decision import _classify, compile_view
from tls_registry import (
    DEFAULT_DRAIN_TIMEOUT_SECONDS, AdmissionResult, BoundConnectionRegistry, RequestAdmission,
)

# Same cadence as the sidecar drain thread; the page size is the registry maximum.
_DRAIN_INTERVAL_SECONDS = 0.5
_DRAIN_PAGE_LIMIT = 128
# Lifecycle teardown waits at most this long for confirmed disconnects; the
# Go lifecycle command deadline is 2s and includes the IPC round trip.
TEARDOWN_TIMEOUT_SECONDS = 1.0
_TEARDOWN_RETRY_SECONDS = 0.1


def _source(client):
    """Canonical peer address, or None when it cannot select any subject."""
    from subject_revision import normalize_source
    try:
        return normalize_source(client.peername[0])
    except (AttributeError, IndexError, TypeError, ValueError):
        return None


@dataclass(eq=False, slots=True)
class _Member:
    """One live proxied client and the identity fixed when it connected."""

    client: object = field(repr=False)
    address: str | None
    target: object
    registry: object = field(repr=False)
    doomed: bool = False


@dataclass(frozen=True, eq=False, slots=True)
class Teardown:
    """Exact doomed members plus the source fence held until release()."""

    members: tuple
    addresses: tuple
    everything: bool


class SubjectRegistry(BoundConnectionRegistry):
    """One entry lifetime; invalidation never releases a live transport token."""

    def __init__(self, *, capacity, accounting, static_passthrough,
                 drain_timeout_seconds=DEFAULT_DRAIN_TIMEOUT_SECONDS):
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
    whose registry retirement deadline expired. Separately, every proxied
    client is indexed from client_connected with the target and registry its
    source owned at that moment; lifecycle teardown dooms and closes those
    exact members and never lets a client inherit a later Register.
    """

    def __init__(self, owner):
        self.owner = owner
        self._lock = threading.Lock()
        self._connections = {}
        self._tokens = {}
        self._clients = {}
        self._fenced = {}
        self._fence_all = 0
        self._changed = threading.Condition(self._lock)
        # (event loop, proxyserver addon), captured inside a TLS hook; the
        # drain thread only schedules closure onto that loop.
        self._transport = (None, None)
        self._drain_stop = threading.Event()
        self._drain_thread = None

    def client_connected(self, client):
        """Fix the connecting source identity; reject it while that source is fenced."""
        self._capture_transport()
        address = _source(client)
        with self._lock:
            if id(client) in self._clients:
                return
            if self._fence_all or self._fenced.get(address):
                rejected = True
            else:
                rejected = False
                # Lock-free index read under our lock: a lifecycle fence
                # publishes the revoked index before it takes this lock.
                dispatch = None if address is None else self.owner.lookup_source(address)
                self._clients[id(client)] = _Member(
                    client, address, None if dispatch is None else dispatch.target,
                    None if dispatch is None else dispatch.registry)
        if rejected:
            # mitmproxy closes a client whose connect hook sets an error.
            client.error = "connection fenced"

    def admit_tls(self, client, sni, *, ech_hidden=False, destination_port=None):
        with self._lock:
            member = self._clients.get(id(client))
            registry = None
            if member is not None and member.client is client and not member.doomed:
                dispatch = None if member.address is None else self.owner.lookup_source(member.address)
                if (member.target is not None and dispatch is not None
                        and dispatch.target == member.target and dispatch.registry is member.registry):
                    registry = member.registry
        if registry is None:
            # Unindexed, doomed, ownerless or reassigned sources never adopt
            # the current source owner.
            return AdmissionResult("deny", "unknown_identity")
        result = registry.admit(
            identity=(self.owner.control_generation, member.target.subject_generation),
            sni=sni, ech_hidden=ech_hidden, static_passthrough=registry.static_selectors,
            destination_port=destination_port)
        if result.token is not None:
            self._capture_transport()
            with self._lock:
                previous = self._connections.get(id(client))
                # No renegotiation may replace or prematurely release a live token.
                rejected = previous is not None or member.doomed
                if not rejected:
                    item = (registry, result.token, client)
                    self._connections[id(client)] = item
                    self._tokens[id(result.token)] = item
            if rejected:
                registry.release(result.token)
                return AdmissionResult("deny", "invalid_input")
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
            member = self._clients.get(id(item[2]))
            if member is not None and member.doomed:
                return RequestAdmission("deny", "connection_fenced")
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
            member = self._clients.get(id(client))
            if member is not None and member.client is client:
                del self._clients[id(client)]
                self._changed.notify_all()
            item = self._connections.get(id(client))
            if item is None or item[2] is not client:
                return
            del self._connections[id(client)]
            del self._tokens[id(item[1])]
        registry, token, _ = item
        registry.release(token)
        self._observe(registry)

    # -- Lifecycle teardown --------------------------------------------------

    def fence(self, *, targets=(), addresses=(), everything=False):
        """Doom current members and fence new connects; never closes or waits.

        Called by the owner under its operation lock after it revoked routing.
        """
        with self._lock:
            members = tuple(member for member in self._clients.values()
                            if everything or member.target in targets or member.address in addresses)
            for member in members:
                member.doomed = True
            for address in addresses:
                self._fenced[address] = self._fenced.get(address, 0) + 1
            self._fence_all += bool(everything)
        return Teardown(members, tuple(addresses), everything)

    def release(self, ticket):
        """Lift a ticket's source fence; doomed members stay doomed."""
        with self._lock:
            for address in ticket.addresses:
                if self._fenced[address] == 1:
                    del self._fenced[address]
                else:
                    self._fenced[address] -= 1
            self._fence_all -= bool(ticket.everything)

    def live_members(self, addresses):
        """Live members on these sources, of any identity or none."""
        with self._lock:
            return sum(member.address in addresses for member in self._clients.values())

    def teardown(self, ticket, timeout=None):
        """Close exact members on the proxy loop; return (closed, remaining).

        Only client_disconnected confirms closure. Scheduling a callback is not
        completion; a missing loop/handler, an error or the deadline leaves the
        member counted as remaining. On the proxy loop itself, close inline and
        do not wait for disconnect hooks that need this same loop.
        """
        timeout = TEARDOWN_TIMEOUT_SECONDS if timeout is None else timeout
        deadline = time.monotonic() + timeout
        loop, proxyserver = self._transport
        try:
            inline = asyncio.get_running_loop() is loop
        except RuntimeError:
            inline = False
        # Bounded by attempts as well as the clock: no unbounded close loop.
        for _ in range(int(timeout / _TEARDOWN_RETRY_SECONDS) + 1):
            with self._lock:
                pending = [member for member in ticket.members
                           if self._clients.get(id(member.client)) is member]
                if not pending or loop is None:
                    break
            for member in pending:
                if inline:
                    self._teardown_in_loop(proxyserver, member, member.client)
                else:
                    with suppress(Exception):
                        loop.call_soon_threadsafe(
                            self._teardown_in_loop, proxyserver, member, member.client)
            left = deadline - time.monotonic()
            if inline or left <= 0:
                break
            with self._changed:
                # Releases our lock while waiting; reschedule once per interval.
                self._changed.wait_for(
                    lambda: all(self._clients.get(id(member.client)) is not member
                                for member in pending),
                    min(left, _TEARDOWN_RETRY_SECONDS))
        with self._lock:
            remaining = sum(self._clients.get(id(member.client)) is member
                            for member in ticket.members)
        return len(ticket.members) - remaining, remaining

    def _teardown_in_loop(self, proxyserver, member, client):
        """Close only if this exact doomed member is still live; idempotent."""
        with suppress(Exception):
            with self._lock:
                if self._clients.get(id(client)) is not member or not member.doomed:
                    return
            handler = proxyserver.connections.get(client.id)
            if handler is not None:
                handler.close_connection(client)

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
                    if client is not None and self._request_close(registry, token, client):
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

    def _request_close(self, registry, token, client):
        """Schedule closure on the proxy loop; never holds a runtime lock."""
        loop, proxyserver = self._transport
        if loop is None:
            return False
        try:
            loop.call_soon_threadsafe(self._close_in_loop, proxyserver, registry, token, client)
        except Exception:  # noqa: BLE001 - a closed loop is retried next sweep
            return False
        return True

    def _close_in_loop(self, proxyserver, registry, token, client):
        """Recheck the exact retirement inside the proxy loop before closing."""
        with suppress(Exception):
            if self._member_client(registry, token) is not client:
                return
            # Request completion while this callback was queued can remove the
            # last expired retirement or leave only a later deadline.
            state = registry.drain_state(token)
            if state is None or state.deadline is None or state.deadline > time.monotonic():
                return
            handler = proxyserver.connections.get(client.id)
            if handler is not None:
                handler.close_connection(client)


_runtime = None


def runtime():
    return _runtime
