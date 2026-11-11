# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""B3b-1 handshake experiment. HTTP/H2 requests stay disabled until B3b-2.

Only dispatch snapshots feed selector publication. No receiver readback/acquire,
IPC, or owner operation lock is reachable from a ClientHello. B4 opaque teardown
is still missing: this gate must not be enabled in production before B4.
"""

from __future__ import annotations

import threading
from contextlib import suppress

from static_passthrough import StaticPassthrough
from tls_decision import _classify, compile_view
from tls_registry import AdmissionResult, BoundConnectionRegistry


class SubjectRegistry(BoundConnectionRegistry):
    """One entry lifetime; invalidation never releases a live transport token."""

    def __init__(self, *, capacity, accounting, static_passthrough):
        super().__init__(capacity=capacity, accounting=accounting)
        self._static_config = StaticPassthrough(static_passthrough)
        self._dispatch_revision = None

    def publish_dispatch(self, snapshot):
        revision = None if snapshot is None else snapshot.revision
        if revision == self._dispatch_revision:
            return
        # Compilation is on the install writer, before the short registry lock.
        view = None if snapshot is None else compile_view(snapshot)
        with self._lock:
            if view is None:
                self._view = None
            else:
                self._publish_activation(self._plan_activation(view))
            self._dispatch_revision = revision

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
        static_passthrough=registry._static_config.selectors,
        destination_port=destination_port,
    ), registry


class SubjectAdmissionRuntime:
    """Connection ownership stays pinned to the handshake registry across IP reuse.

    B3b-2 consumes these exact (registry, token, client) memberships for request
    admission and verified teardown. No registry slot is released on Unload or
    suspend; client_disconnected is the terminal signal in this intermediate.
    """

    def __init__(self, owner):
        self.owner = owner
        self._lock = threading.Lock()
        self._connections = {}

    def admit_tls(self, client, sni, *, ech_hidden=False, destination_port=None):
        try:
            address = client.peername[0]
        except (AttributeError, IndexError, TypeError):
            address = ""
        result, registry = admit_source(self.owner, address, sni,
                                       ech_hidden=ech_hidden, destination_port=destination_port)
        if result.token is not None:
            with self._lock:
                previous = self._connections.get(id(client))
                # No renegotiation may replace or prematurely release a live token.
                if previous is None:
                    self._connections[id(client)] = (registry, result.token, client)
            if previous is not None:
                registry.release(result.token)
                return AdmissionResult("deny", "invalid_input")
        if registry is not None:
            self._observe(registry)
        return result

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
        registry, token, _ = item
        registry.release(token)
        self._observe(registry)


_runtime = None


def runtime():
    return _runtime
