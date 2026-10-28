# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""P2 test observer; never an adapter or publication owner.

Load AFTER the actual system.py. Its only scenario faults are an explicitly
named async ServerConnectedHook gate and a one-stream barrier immediately
before the real LiveRuntime admission method. The barrier delegates to that
same method after parent release. It never closes a writer, changes library
queues, changes destinations, or alters shared production code.
"""

from __future__ import annotations

import asyncio
import json
import os
import threading
import time
from pathlib import Path

import credential_bound

_DIRECTORY = Path(os.environ["H2_PRODUCTION_OBSERVATIONS"])
_SCENARIO = os.environ.get("H2_PRODUCTION_SCENARIO", "normal")
_sequence = 0
_task = None
_last = None
_barrier_release_value = os.environ.get("H2_PRODUCTION_BARRIER_RELEASE")
_barrier_release = None if _barrier_release_value is None else Path(_barrier_release_value)
_barrier_used = False
_barrier_stop = threading.Event()


def emit(kind, **metadata):
    global _sequence
    _sequence += 1
    value = {"sequence": _sequence, "monotonic_ns": time.monotonic_ns(),
             "kind": kind, **metadata}
    with (_DIRECTORY / "proxy-events.jsonl").open("a", encoding="utf-8") as output:
        output.write(json.dumps(value, sort_keys=True) + "\n")


def publication_epoch(live):
    with live.registry._lock:
        view = live.registry._view
        return None if view is None else view.revision.decision_epoch


def snapshot():
    live = credential_bound.runtime()
    if live is None:
        return {"runtime": False}
    epoch = publication_epoch(live)
    connections = []
    for client, adapter in tuple(live._h2.values()):
        state = live.registry.drain_state(adapter.token)
        # Read deadline metadata under its owner lock; never retain snapshots.
        with live.registry._lock:
            records = [
                {"stream_id": sid,
                 "epoch": record.handle.revision.decision_epoch,
                 "application_done": record.application_done,
                 "request_deadline": live.registry._request_deadlines.get(record.handle.serial),
                 "response_done": record.response_done}
                for sid, record in adapter._active.items()
            ]
        handler = live._proxyserver.connections.get(client.id)
        chain = [] if handler is None else [
            type(node).__name__ for node in credential_bound._layer_chain(handler.layer)
        ]
        connections.append({
            "publication_epoch": epoch,
            "connection_serial": adapter.token.serial,
            "adapter": type(adapter).__name__,
            "layer_chain": chain,
            "destination_port": adapter.http_layer.context.server.address[1],
            "client_h2": client.alpn == b"h2",
            "active": records,
            "pending_count": len(adapter._pending),
            "claimed_count": len(adapter._claimed),
            "refused_count": len(adapter._refused),
            "failed": adapter.failed,
            "goaway_sent": adapter.goaway_sent,
            "client_closed": adapter.client_closed,
            "fenced": None if state is None else state.fenced,
            "frozen": None if state is None else state.frozen_last_stream_id,
            "deadline": None if state is None else state.deadline,
            "high_water": None if state is None else state.max_admitted_stream_id,
        })
    if publication_epoch(live) != epoch:
        return snapshot()  # discard a sample that crossed the atomic publication
    return {"runtime": True, "publication_epoch": epoch, "connections": connections,
            "registry_connections": live.registry.count,
            "registry_requests": live.registry.request_count,
            "runtime_connections": len(live._connections),
            "adapters": len(live._h2),
            "upstream_tasks": len(live._h2_transports)}


async def observe():
    global _last
    last_emitted = 0.0
    while True:
        current = snapshot()
        now = time.monotonic()
        if current != _last or now - last_emitted >= 0.1:
            emit("state", **current)
            _last = current
            last_emitted = now
        await asyncio.sleep(0.02)


def running():
    global _task
    if not _DIRECTORY.is_dir():
        raise RuntimeError("P2 observation directory is missing")
    emit("observer_ready", scenario=_SCENARIO)
    if _SCENARIO == "pre_admission_barrier_h2":
        live = credential_bound.runtime()
        if live is None or _barrier_release is None:
            raise RuntimeError("pre-admission test barrier has no live runtime or release path")
        original = live.acquire_request

        def gated_acquire(token, *, client_stream_id=None):
            global _barrier_used
            if client_stream_id == 3 and not _barrier_used:
                _barrier_used = True
                emit("pre_admission_barrier_entered", stream_id=client_stream_id,
                     before_registry_acquire=True)
                deadline = time.monotonic() + 20
                while not _barrier_release.exists() and time.monotonic() < deadline:
                    if _barrier_stop.wait(0.01):
                        break
                if not _barrier_release.exists():
                    emit("pre_admission_barrier_timeout", stream_id=client_stream_id)
                    raise RuntimeError("pre-admission test barrier timed out")
                emit("pre_admission_release_observed", stream_id=client_stream_id,
                     before_registry_acquire=True)
                admission = original(token, client_stream_id=client_stream_id)
                state = live.registry.drain_state(token)
                emit("pre_admission_result", stream_id=client_stream_id,
                     action=admission.action, reason=admission.reason,
                     handle_created=admission.handle is not None,
                     fenced=None if state is None else state.fenced,
                     frozen=None if state is None else state.frozen_last_stream_id,
                     registry_request_count=live.registry.request_count)
                return admission
            return original(token, client_stream_id=client_stream_id)

        live.acquire_request = gated_acquire
    _task = asyncio.create_task(observe())


def done():
    _barrier_stop.set()
    if _task is not None:
        _task.cancel()


def requestheaders(flow):
    live = credential_bound.runtime()
    adapter = None if live is None else live.h2_for(flow.client_conn)
    handle = flow.metadata.get("opensandbox_credential_request_handle")
    values = flow.request.headers.get_all("x-h2-logical-id")
    logical_id = values[0] if len(values) == 1 and values[0] in ("old", "new") else None
    state = None if adapter is None else live.registry.drain_state(adapter.token)
    emit("requestheaders", attached=adapter is not None,
         logical_id=logical_id,
         connection_serial=None if adapter is None else adapter.token.serial,
         fenced=None if state is None else state.fenced,
         stream_id=None if handle is None else handle.client_stream_id,
         epoch=None if handle is None else handle.revision.decision_epoch,
         admitted=handle is not None,
         private_token_present=bool(flow.request.headers.get_all("private-token")))


def response(flow):
    handle = flow.metadata.get("opensandbox_credential_request_handle")
    emit("response_hook", stream_id=None if handle is None else handle.client_stream_id,
         epoch=None if handle is None else handle.revision.decision_epoch,
         retained=handle is not None)


async def server_connected(data):
    if _SCENARIO != "d3_pending_server_connected":
        return
    live = credential_bound.runtime()
    adapter = None if live is None else live.h2_for(data.client)
    handler = None if live is None else live._proxyserver.connections.get(data.client.id)
    current = None if handler is None else handler.transports.get(data.server)
    writer = None if current is None else current.writer
    valid = (adapter is not None and current is not None
             and current.handler is asyncio.current_task()
             and isinstance(writer, asyncio.StreamWriter) and not writer.is_closing())
    emit("d3_hook_gated", attached=adapter is not None,
         actual_tcp_writer=valid, task_pending=current is not None and not current.handler.done())
    if not valid:
        raise RuntimeError("P2 D3 gate lacks an actual owned TCP writer")
    # Cancellation here reproduces the pinned dependency's narrow cleanup gap.
    # There is intentionally no writer.close(), teardown or state mutation.
    await asyncio.Event().wait()
