# Copyright 2026 The OpenSandbox Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# http://www.apache.org/licenses/LICENSE-2.0
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Production LiveRuntime D3 lifecycle with real pinned dependency tasks/layers.

The synthetic event loop opens no sockets. Its real StreamWriters use a memory
transport. This is pure scheduling evidence, not a real socket/launcher test.
Hard exit is intercepted in every host test; only the explicit isolated-child
entry point is permitted to execute it. Dependency classes are never patched.
"""

from __future__ import annotations

import asyncio
import hashlib
import importlib.util
import json
import os
import subprocess
import sys
import unittest
from pathlib import Path
from types import SimpleNamespace
from typing import Optional
from unittest import mock

MITMSCRIPTS = Path(__file__).resolve().parents[1] / "mitmscripts"
sys.path.insert(0, str(MITMSCRIPTS))
import credential_bound
import h2.config
import h2.connection
from credential_bound import LiveRuntime
from mitmproxy import connection, ctx, http
from mitmproxy.addons.next_layer import NextLayer as NextLayerSelector
from mitmproxy.addons.proxyserver import Proxyserver
from mitmproxy.options import Options
from mitmproxy.proxy import commands, events, layer, server, server_hooks
from mitmproxy.proxy.context import Context
from mitmproxy.proxy.layers import http as layers
from mitmproxy.proxy.layers.http._http2 import Http2Server
from mitmproxy.proxy.layers.modes import TransparentProxy
from mitmproxy.proxy.layers.tls import ClientTLSLayer, TlsStartServerHook
from OpenSSL import SSL
from revision_publication import LiveReceiver
from revision_receiver import Revision


def options():
    options = Options(http2_ping_keepalive=0)
    for name, typ, default in [
        ("validate_inbound_headers", bool, True),
        ("normalize_outbound_headers", bool, True),
        ("stream_large_bodies", Optional[str], None),  # noqa: UP045 - pinned Options API
        ("body_size_limit", Optional[str], None),  # noqa: UP045 - pinned Options API
        ("connection_strategy", str, "lazy"),
        ("keep_host_header", bool, False),
    ]:
        options.add_option(name, typ, default, "Pure lifecycle test option")
    return options


def make_runtime(proxyserver, loop):
    receiver = LiveReceiver(
        "control",
        "subject",
        max_snapshot_bytes=1 << 20,
        capacity=64,
        request_capacity=64,
    )
    payload = json.dumps(
        {
            "version": 1,
            "vaultRevision": 1,
            "effectivePolicyEpoch": 0,
            "interceptionMode": "credential-bound",
            "state": "active",
            "tlsBindingHostSelectors": ["example.test"],
            "fullRenderedBindings": [
                {
                    "name": "synthetic",
                    "match": {
                        "schemes": ["https"],
                        "hosts": ["example.test"],
                        "methods": ["POST"],
                        "paths": ["/*"],
                    },
                    "headers": [
                        {"name": "Authorization", "value": "synthetic-test-value"}
                    ],
                }
            ],
            "redactions": ["synthetic-test-value"],
        },
        separators=(",", ":"),
    ).encode()
    revision = Revision(
        "control", "subject", 1, 1, 0, hashlib.sha256(payload).hexdigest()
    )
    receiver.prepare(revision, payload)
    receiver.commit(revision)
    runtime = LiveRuntime(receiver, ("control", "subject"))
    runtime._proxyserver = proxyserver
    runtime._loop = loop
    return runtime


class ReadyScheduler:
    def __init__(self, loop_type=asyncio.BaseEventLoop):
        # BaseEventLoop creates no selector, wakeup socket, or I/O transport.
        self.loop = loop_type()
        self.errors = []
        self.loop.set_exception_handler(
            lambda loop, context: self.errors.append(context)
        )
        self.previous = asyncio.events._get_running_loop()
        asyncio.events._set_running_loop(self.loop)

    def one(self):
        if self.loop._ready:
            self.loop._ready.popleft()._run()
            assert not self.errors, self.errors
            return True
        return False

    def ready(self):
        budget = 1000
        while self.one():
            budget -= 1
            assert budget > 0, "Unexpected busy loop"

    def now(self, coro):
        # server_event contains no await after its unblocked lock acquisition.
        try:
            value = coro.send(None)
        except StopIteration as stop:
            return stop.value
        else:
            coro.close()
            raise AssertionError(f"Unexpected async suspension: {value!r}")

    def close(self):
        for task in asyncio.all_tasks(self.loop):
            task.cancel()
        self.ready()
        assert not asyncio.all_tasks(self.loop)
        self.loop.close()
        asyncio.events._set_running_loop(self.previous)


class ClientMemoryWriter:
    def __init__(self, peer):
        self.peer = peer
        self.closed = False
        self.bytes = 0

    def is_closing(self):
        return self.closed

    def write(self, data):
        self.bytes += len(data)
        self.peer.receive_data(data)

    def close(self):
        self.closed = True


class BaseHandler(server.ConnectionHandler):
    def __init__(self, context, gate):
        super().__init__(context)
        self.hooks = []
        self.hook_gate = gate
        self.flows = {}

    async def handle_hook(self, hook):
        self.hooks.append(hook.name)
        if isinstance(hook, layers.HttpRequestHeadersHook):
            f = hook.flow
            f.request.stream = True
            self.flows[int(f.request.path.strip("/"))] = f
        if (
            isinstance(hook, server_hooks.ServerConnectHook)
            and self.hook_gate is not None
        ):
            await self.hook_gate
        if (
            isinstance(hook, server_hooks.ServerConnectedHook)
            and getattr(self, "connected_gate", None) is not None
        ):
            await self.connected_gate

    def log(self, *args, **kwargs):
        pass


class SyntheticTransport(asyncio.Transport):
    """Test transport supplied by a test event loop. No socket or real I/O."""

    def __init__(self, loop, protocol):
        self.loop = loop
        self.protocol = protocol
        self.closing = False
        self.sent = []

    def get_extra_info(self, name, default=None):
        return {"peername": ("127.0.0.1", 443), "sockname": ("127.0.0.1", 50001)}.get(
            name, default
        )

    def is_closing(self):
        return self.closing

    def write(self, data):
        self.sent.append(data)

    def close(self):
        if getattr(self, "close_fails", False):
            raise OSError("synthetic writer close failure")
        self.closing = True
        self.loop.call_soon(self.protocol.connection_lost, None)

    def abort(self):
        self.close()


class MemoryConnectLoop(asyncio.BaseEventLoop):
    """Supported asyncio loop implementation for deterministic synthetic connect."""

    async def create_connection(self, protocol_factory, *args, **kwargs):
        protocol = protocol_factory()
        transport = SyntheticTransport(self, protocol)
        protocol.connection_made(transport)
        return transport, protocol


class FailingConnectLoop(MemoryConnectLoop):
    async def create_connection(self, protocol_factory, *args, **kwargs):
        raise OSError("synthetic TCP connection refused")


class LifecycleAdapter(Http2Server):
    """Concrete instance fixture; the lifecycle under test is unmodified runtime."""

    def __init__(self, context, runtime, root, token):
        super().__init__(context)
        self.runtime = runtime
        self.http_layer = root
        self.token = token
        self.client_closed = False
        self.failed = False
        self.active_upstreams = []
        self.close_request = None
        self.wakeup = None
        self.wake_only = False
        self.graceful_close = False

    def owned_upstreams(self):
        return tuple(self.active_upstreams)

    def fail_closed(self):
        self.failed = True

    def on_client_disconnected(self):
        self.client_closed = True
        self.wakeup = None

    def handle_event(self, event):
        if not (isinstance(event, events.Wakeup) and event.command is self.wakeup):
            yield from super().handle_event(event)
        if self.wake_only:
            self.wake_only = False
            self.wakeup = commands.RequestWakeup(600)
            yield self.wakeup
        if self.graceful_close:
            self.graceful_close = False
            self.h2_conn.close_connection(error_code=0, last_stream_id=1)
            yield commands.SendData(self.context.client, self.h2_conn.data_to_send())
            yield commands.CloseConnection(self.context.client)
        if self.close_request is not None:
            item, self.close_request = self.close_request, None
            observation = self.runtime.prepare_h2_close(
                self, item.connection, item.owner
            )
            yield commands.CloseConnection(item.connection)
            self.runtime.h2_close_consumed(observation)
            yield commands.CloseConnection(self.context.client)
            self.wakeup = commands.RequestWakeup(600)
            yield self.wakeup


class FixtureHandler(BaseHandler):
    async def handle_hook(self, hook):
        if isinstance(hook, server_hooks.ServerConnectHook):
            self.runtime.server_connect(hook.data)
        if isinstance(hook, server_hooks.ServerConnectedHook):
            self.runtime.server_connected(hook.data)
        if isinstance(hook, server_hooks.ServerConnectErrorHook):
            self.runtime.server_connect_error(hook.data)
        if isinstance(hook, server_hooks.ServerDisconnectedHook):
            self.runtime.server_disconnected(hook.data)
        if isinstance(hook, layer.NextLayerHook):
            hook.data.layer = self.http_root
        if isinstance(hook, server_hooks.ClientDisconnectedHook):
            self.wakes_cancelled_at_client_hook = not self.wakeup_timer
            self.runtime.client_disconnected(self.client)
            if getattr(self, "after_client_disconnect", None) is not None:
                self.after_client_disconnect.set_result(None)
        if isinstance(hook, server_hooks.ServerConnectedHook) and getattr(
            self, "delay_cancel", False
        ):
            self.hooks.append(hook.name)
            try:
                await self.connected_gate
            except asyncio.CancelledError:
                await self.after_client_disconnect
                raise
            return
        await super().handle_hook(hook)
        if (
            isinstance(hook, layers.HttpRequestHeadersHook)
            and hook.flow.request.path == "/3"
            and getattr(self, "late_gate", None) is not None
        ):
            await self.late_gate
            self.late_admissions.append(
                self.runtime.acquire_request(self.late_token, client_stream_id=3)
            )


class FailingCloseHandler(FixtureHandler):
    def close_connection(self, conn, half_close=False):
        if isinstance(conn, connection.Server):
            raise RuntimeError("synthetic normal close rejection")  # noqa: TRY004
        return super().close_connection(conn, half_close)


class MemoryClientWriter(ClientMemoryWriter):
    async def drain(self):
        pass


class Fixture:
    def __init__(
        self,
        *,
        full_lifecycle=False,
        delay_cancel=False,
        process=None,
        sched=None,
        upstream_tls=True,
        handler_type=FixtureHandler,
        real_exit=False,
        observe_before_start=True,
        loop_type=MemoryConnectLoop,
    ):
        self.own_scheduler = sched is None
        self.sched = sched or ReadyScheduler(loop_type)
        self.exits = []
        self.proxyserver = process._proxyserver if process else Proxyserver()
        self.process = process or make_runtime(self.proxyserver, self.sched.loop)
        self.exit_patch = None
        if process is None and not real_exit:
            self.exit_patch = mock.patch.object(
                credential_bound.os, "_exit", self.exits.append
            )
            self.exit_patch.start()
        self.ctx = Context(
            connection.Client(
                peername=("127.0.0.1", 50000),
                sockname=("127.0.0.1", 443),
                state=connection.ConnectionState.OPEN,
            ),
            options(),
        )
        self.ctx.client.alpn = b"h2"
        self.ctx.client.tls = True
        self.ctx.client.sni = "example.test"
        self.ctx.server.address = ("example.test", 443)
        self.ctx.server.tls = upstream_tls
        self.handler = handler_type(self.ctx, None)
        self.handler.runtime = self.process
        admitted = self.process.admit_tls("example.test")
        assert admitted.token is not None
        assert self.process.track(admitted.token, self.ctx.client)
        self.handler.connected_gate = self.sched.loop.create_future()
        self.handler.delay_cancel = delay_cancel
        if delay_cancel:
            self.handler.after_client_disconnect = self.sched.loop.create_future()
        self.root = layers.HttpLayer(self.ctx, layers.HTTPMode.transparent)
        self.handler.http_root = self.root
        self.adapter = LifecycleAdapter(
            self.ctx.fork(), self.process, self.root, admitted.token
        )
        self.root.connections[self.ctx.client] = self.adapter
        self.process.attach_h2(self.ctx.client, self.adapter)
        self.peer = h2.connection.H2Connection(
            h2.config.H2Configuration(client_side=True)
        )
        self.peer.initiate_connection()
        self.client_reader = asyncio.StreamReader(loop=self.sched.loop)
        self.handler.transports[self.ctx.client] = server.ConnectionIO(
            reader=self.client_reader, writer=MemoryClientWriter(self.peer)
        )
        self.registration = None
        self.deregistered = False
        self.exit_seen_before_deregister = False
        self.full_lifecycle = full_lifecycle
        if full_lifecycle:

            async def registered_client():
                with self.proxyserver.register_connection(
                    self.ctx.client.id, self.handler
                ):
                    await self.handler.handle_client()
                    self.exit_seen_before_deregister = self.process._h2_exit_started
                self.deregistered = True

            self.client_lifecycle = self.sched.loop.create_task(registered_client())
            self.sched.ready()
        else:
            self.registration = self.proxyserver.register_connection(
                self.ctx.client.id, self.handler
            )
            self.registration.__enter__()

            async def hold_client():
                await self.sched.loop.create_future()

            self.handler.transports[
                self.ctx.client
            ].handler = self.sched.loop.create_task(hold_client())
            self.sched.now(self.handler.server_event(events.Start()))
            self.sched.ready()
        self.send(self.peer.data_to_send())
        self.peer.send_headers(
            1,
            [
                (":method", "POST"),
                (":scheme", "https"),
                (":authority", "example.test"),
                (":path", "/1"),
            ],
            end_stream=False,
        )
        self.send(self.peer.data_to_send())
        while len(self.handler.transports) < 2:
            assert self.sched.one()
        self.flow = self.handler.flows[1]
        # The exact pending GetHttpConnection is authoritative while flow.server_conn
        # still points at the original transparent destination.
        owned = [
            (conn, self.root.connections[conn])
            for conn, waiting in self.root.waiting_for_establishment.items()
            for command in waiting
            if self.root.command_sources[command].flow is self.flow
        ]
        assert len(owned) == 1
        self.adapter.active_upstreams = owned
        if observe_before_start:
            self.item = self.process._observe_h2(self.adapter, *owned[0])
        else:
            self.sched.ready()
            self.item = next(iter(self.process._h2_transports.values()))
        admission = self.process.acquire_request(self.item.token, client_stream_id=1)
        assert admission.action == "allow"
        self.request_handle = admission.handle

    def send(self, data):
        if self.full_lifecycle:
            self.client_reader.feed_data(data)
        else:
            self.sched.now(
                self.handler.server_event(events.DataReceived(self.ctx.client, data))
            )

    def establish(self):
        self.sched.ready()
        assert "server_connected" in self.handler.hooks
        assert self.handler.transports[self.item.connection].writer is not None

    def close(self):
        self.adapter.close_request = self.item
        self.sched.now(
            self.handler.server_event(events.DataReceived(self.ctx.client, b""))
        )

    def cleanup(self):
        # Fixture cleanup only. Production never closes a writer directly.
        for io in list(self.handler.transports.values()):
            if io.writer is not None:
                if isinstance(io.writer, asyncio.StreamWriter):
                    io.writer.transport.close_fails = False
                io.writer.close()
        if self.own_scheduler:
            self.sched.close()
        if self.registration is not None:
            self.registration.__exit__(None, None, None)
        if self.exit_patch is not None:
            self.exit_patch.stop()


class H2RuntimeLifecycleTest(unittest.TestCase):
    def fixture(self, **kwargs):
        fixture = Fixture(**kwargs)
        self.addCleanup(fixture.cleanup)
        return fixture

    def assert_released(self, f):
        self.assertIsNone(f.process.token_for(f.ctx.client))
        self.assertIsNone(f.process.h2_for(f.ctx.client))
        self.assertIsNone(f.process._connection_for_serial(f.item.token.serial))
        self.assertEqual(f.process.registry.count, 0)
        self.assertEqual(f.process.registry.request_count, 0)
        self.assertNotIn(f.item.task, f.process._h2_transports)

    def assert_retained(self, f):
        self.assertIs(f.process.token_for(f.ctx.client), f.item.token)
        self.assertIs(f.process.h2_for(f.ctx.client), f.adapter)
        self.assertEqual(f.process.registry.count, 1)
        self.assertEqual(f.process.registry.request_count, 1)

    def test_current_io_replacement_fires_exactly_once(self):
        f = self.fixture()
        opening = f.handler.transports[f.item.connection]
        self.assertIsNone(opening.writer)
        f.establish()
        current = f.handler.transports[f.item.connection]
        self.assertIsNot(current, opening)
        self.assertIs(current.handler, f.item.task)
        self.assertIsInstance(current.writer, asyncio.StreamWriter)
        f.close()
        self.assertTrue(f.item.close_consumed)
        self.assertFalse(f.item.task.done())
        self.assertEqual(f.exits, [])
        f.sched.ready()
        self.assertTrue(f.item.task.cancelled())
        self.assertFalse(current.writer.is_closing())
        self.assertEqual(f.exits, [86])
        for _ in range(3):
            f.process._h2_task_done(f.item, f.item.task)
        self.assertEqual(f.exits, [86])

    def test_preconnect_cancellation_is_not_fatal_and_releases(self):
        for when in ("before_start", "server_connect_hook", "semaphore"):
            with self.subTest(when=when):
                f = Fixture()
                try:
                    if when == "server_connect_hook":
                        f.handler.hook_gate = f.sched.loop.create_future()
                    elif when == "semaphore":
                        sem = f.handler.max_conns[f.item.connection.address]
                        for _ in range(5):
                            f.sched.now(sem.acquire())
                    if when != "before_start":
                        f.sched.ready()
                    f.close()
                    f.sched.ready()
                    self.assertTrue(f.item.task.done())
                    self.assertIsNone(f.handler.transports[f.item.connection].writer)
                    self.assertEqual(f.exits, [])
                    f.process.client_disconnected(f.ctx.client)
                    self.assert_released(f)
                finally:
                    f.cleanup()

    def test_pending_task_waits_but_unproven_completed_close_exits(self):
        f = self.fixture()
        f.establish()
        f.process._h2_task_done(f.item, f.item.task)
        self.assertEqual(f.exits, [])
        f.process.prepare_h2_close(f.adapter, f.item.connection, f.item.owner)
        f.item.task.cancel("test cancellation without normal close")
        f.sched.ready()
        self.assertFalse(f.item.close_consumed)
        self.assertEqual(f.exits, [87])
        self.assert_retained(f)

    def test_done_without_exact_connect_error_hook_exits_without_release(self):
        f = self.fixture()
        f.item.task.cancel("test unobserved preconnect cancellation")
        f.sched.ready()
        self.assertTrue(f.item.task.done())
        self.assertFalse(f.item.close_consumed)
        self.assertIsNone(f.item.connect_error_io)
        self.assertFalse(f.item.terminal)
        self.assertTrue(f.process._h2_exit_started)
        self.assertEqual(f.exits, [87])
        self.assert_retained(f)

    def test_actual_connect_error_hook_proves_normal_no_io_completion(self):
        f = self.fixture(loop_type=FailingConnectLoop)
        f.sched.ready()
        self.assertTrue(f.item.task.done())
        self.assertFalse(f.item.task.cancelled())
        self.assertIsNone(f.item.task.exception())
        self.assertIs(f.item.connect_error_io, f.handler.transports[f.item.connection])
        self.assertIsNone(f.item.connection.timestamp_tcp_setup)
        self.assertTrue(f.item.terminal)
        self.assertFalse(f.item.close_consumed)
        self.assertEqual(f.exits, [])
        f.process.client_disconnected(f.ctx.client)
        self.assert_released(f)

    def test_current_closing_writer_is_not_a_safe_release(self):
        f = self.fixture()
        f.establish()
        f.handler.transports[f.item.connection].writer.close()  # fixture state
        f.close()
        f.sched.ready()
        self.assertEqual(f.exits, [87])
        self.assertFalse(
            f.process.h2_transport_terminal(f.adapter, f.item.connection, f.item.owner)
        )
        self.assert_retained(f)

    def test_normal_close_proves_writer_closed_and_io_removed(self):
        f = self.fixture(upstream_tls=False)
        f.establish()
        f.handler.connected_gate.set_result(None)
        f.sched.ready()
        writer = f.handler.transports[f.item.connection].writer
        f.close()
        f.sched.ready()
        self.assertTrue(f.item.task.done())
        self.assertTrue(writer.is_closing())
        self.assertNotIn(f.item.connection, f.handler.transports)
        self.assertTrue(
            f.process.h2_transport_terminal(f.adapter, f.item.connection, f.item.owner)
        )
        self.assertEqual(f.exits, [])
        f.process.client_disconnected(f.ctx.client)
        self.assert_released(f)

    def test_server_hooks_capture_task_without_adapter_close(self):
        f = self.fixture(observe_before_start=False)
        self.assertFalse(f.item.close_consumed)
        self.assertIsNotNone(f.item.connected_io.writer)
        self.assertIs(f.item.task, f.handler.transports[f.item.connection].handler)
        f.close()
        f.sched.ready()
        self.assertEqual(f.exits, [86])

    def test_swallowed_writer_close_error_is_not_terminal_proof(self):
        f = self.fixture(upstream_tls=False)
        f.establish()
        f.handler.connected_gate.set_result(None)
        f.sched.ready()
        writer = f.handler.transports[f.item.connection].writer
        writer.transport.close_fails = True
        try:
            f.close()
            f.sched.ready()
            self.assertTrue(f.item.task.done())
            self.assertTrue(f.item.disconnected_hook_seen)
            self.assertNotIn(f.item.connection, f.handler.transports)
            self.assertFalse(writer.is_closing())
            self.assertFalse(
                f.process.h2_transport_terminal(
                    f.adapter, f.item.connection, f.item.owner
                )
            )
            self.assertEqual(f.exits, [87])
            f.process.client_disconnected(f.ctx.client)
            self.assert_retained(f)
        finally:
            writer.transport.close_fails = False
            writer.close()  # fixture cleanup only

    def test_stale_admission_ignored_but_current_unknown_ownership_exits(self):
        cases = (
            "proxy_handler",
            "client_identity",
            "handler_layer",
            "adapter_slot",
            "upstream_owner",
            "connection_key",
            "current_task",
            "current_io_missing",
            "runtime_token",
            "runtime_adapter",
            "adapter_root",
            "adapter_runtime",
        )
        for changed in cases:
            with self.subTest(changed=changed):
                f = Fixture()
                removed = None
                try:
                    f.establish()
                    f.close()
                    if changed == "proxy_handler":
                        f.proxyserver.connections[f.ctx.client.id] = FixtureHandler(
                            f.ctx, None
                        )
                    elif changed == "client_identity":
                        f.handler.client = connection.Client(
                            peername=("x", 1), sockname=("y", 2)
                        )
                    elif changed == "handler_layer":
                        f.handler.layer = layer.NextLayer(f.ctx)
                    elif changed == "adapter_slot":
                        f.root.connections[f.ctx.client] = Http2Server(f.ctx.fork())
                    elif changed == "upstream_owner":
                        f.root.connections[f.item.connection] = layer.NextLayer(f.ctx)
                    elif changed == "connection_key":
                        owner = f.root.connections.pop(f.item.connection)
                        replacement = connection.Server(
                            address=f.item.connection.address
                        )
                        f.root.connections[replacement] = owner
                    elif changed == "current_task":

                        async def other(loop=f.sched.loop):
                            await loop.create_future()

                        f.handler.transports[
                            f.item.connection
                        ].handler = f.sched.loop.create_task(other())
                    elif changed == "current_io_missing":
                        removed = f.handler.transports.pop(f.item.connection)
                    elif changed == "runtime_token":
                        new = f.process.admit_tls("example.test").token
                        self.assertTrue(f.process.track(new, f.ctx.client))
                    elif changed == "runtime_adapter":
                        f.process._h2[id(f.ctx.client)] = (
                            f.ctx.client,
                            Http2Server(f.ctx.fork()),
                        )
                    elif changed == "adapter_root":
                        root = layers.HttpLayer(f.ctx, layers.HTTPMode.transparent)
                        root.connections = f.root.connections
                        f.handler.layer = root
                        f.adapter.http_layer = root
                    else:
                        f.adapter.runtime = make_runtime(f.proxyserver, f.sched.loop)
                    f.sched.ready()
                    self.assertTrue(f.item.task.done())
                    self.assertEqual(f.exits, [] if changed in ("runtime_token", "runtime_adapter") else [87])
                    self.assertFalse(f.item.terminal)
                finally:
                    if removed is not None:
                        removed.writer.close()  # fixture cleanup only
                    f.cleanup()

    def test_wrong_done_task_and_loop_do_not_fire(self):
        f = self.fixture()
        f.establish()
        f.close()
        while not f.item.task.done():
            self.assertTrue(f.sched.one())
        other_task = f.sched.loop.create_future()
        other_task.set_result(None)
        f.process._h2_task_done(f.item, other_task)
        self.assertEqual(f.exits, [])
        other_loop = asyncio.BaseEventLoop()
        asyncio.events._set_running_loop(other_loop)
        try:
            f.process._h2_task_done(f.item, f.item.task)
            self.assertEqual(f.exits, [])
        finally:
            asyncio.events._set_running_loop(f.sched.loop)
            other_loop.close()
        f.sched.ready()
        self.assertEqual(f.exits, [86])

    def test_actual_client_lifecycle_callback_precedes_deregistration(self):
        f = self.fixture(full_lifecycle=True, delay_cancel=True)
        f.establish()
        f.close()
        f.sched.ready()
        self.assertTrue(f.client_lifecycle.done())
        self.assertTrue(f.handler.wakes_cancelled_at_client_hook)
        self.assertFalse(f.handler.wakeup_timer)
        self.assertTrue(f.deregistered)
        self.assertTrue(f.exit_seen_before_deregister)
        self.assertTrue(f.item.task.done())
        self.assertEqual(f.exits, [86])
        self.assert_retained(f)

    def test_two_clients_share_one_process_exit_flag(self):
        first = self.fixture()
        second = Fixture(process=first.process, sched=first.sched)
        self.addCleanup(second.cleanup)
        first.establish()
        second.establish()
        first.close()
        second.close()
        first.sched.ready()
        self.assertTrue(first.item.task.done())
        self.assertTrue(second.item.task.done())
        self.assertEqual(first.exits, [86])

    def test_routine_peer_reset_does_not_arm_fatal(self):
        f = self.fixture()
        f.establish()
        f.peer.reset_stream(1, error_code=8)
        f.send(f.peer.data_to_send())
        f.sched.ready()
        self.assertFalse(f.item.close_consumed)
        self.assertEqual(f.exits, [])

    def test_natural_disconnect_fences_late_hook_and_closes_upstream(self):
        f = self.fixture(full_lifecycle=True, delay_cancel=True)
        f.establish()
        f.handler.late_gate = f.sched.loop.create_future()
        f.handler.late_admissions = []
        f.handler.late_token = f.item.token
        f.peer.send_headers(
            3,
            [
                (":method", "POST"),
                (":scheme", "https"),
                (":authority", "example.test"),
                (":path", "/3"),
            ],
            end_stream=False,
        )
        f.send(f.peer.data_to_send())
        f.sched.ready()
        self.assertFalse(f.item.close_consumed)
        f.client_reader.feed_eof()
        f.sched.ready()
        self.assertTrue(f.handler.wakes_cancelled_at_client_hook)
        self.assertTrue(f.item.close_consumed)
        self.assertEqual(f.exits, [86])
        self.assert_retained(f)
        f.handler.late_gate.set_result(None)
        f.sched.ready()
        self.assertEqual(len(f.handler.late_admissions), 1)
        denied = f.handler.late_admissions[0]
        self.assertEqual((denied.action, denied.reason), ("deny", "connection_fenced"))
        self.assertEqual(f.process.registry.request_count, 1)

    def test_live_registry_denies_late_lower_stream_below_frozen_high_water(self):
        """Real LiveRuntime/Registry owner ordering; no wire or hook claim."""
        f = self.fixture()
        token = f.item.token
        # Stream 3 is held at the pre-acquire point; no handle is fabricated.
        paused_stream_id = 3

        higher = f.process.acquire_request(token, client_stream_id=5)
        self.assertEqual((higher.action, higher.reason), ("allow", "admitted"))
        self.assertIsNotNone(higher.handle)
        self.assertEqual(higher.handle.client_stream_id, 5)
        self.assertEqual(
            f.process.registry.drain_state(token).max_admitted_stream_id, 5
        )

        old_snapshot = f.process._receiver.acquire()
        self.assertIsNotNone(old_snapshot)
        payload = json.loads(old_snapshot.payload)
        payload.update(
            state="active-empty",
            vaultRevision=2,
            tlsBindingHostSelectors=[],
            fullRenderedBindings=[],
            redactions=[],
        )
        encoded = json.dumps(payload, separators=(",", ":")).encode()
        revision = Revision(
            "control", "subject", 2, 2, 0, hashlib.sha256(encoded).hexdigest()
        )
        receiver = f.process._receiver
        self.assertEqual(receiver.prepare(revision, encoded), revision)
        self.assertEqual(receiver.commit(revision), revision)
        self.assertEqual(receiver.readback(), revision)

        state = f.process.registry.drain_state(token)
        self.assertTrue(state.fenced)
        self.assertEqual(state.frozen_last_stream_id, 5)
        self.assertLessEqual(paused_stream_id, state.frozen_last_stream_id)
        self.assertEqual(f.request_handle.snapshot.revision.decision_epoch, 1)
        self.assertEqual(higher.handle.snapshot.revision.decision_epoch, 1)

        late = f.process.acquire_request(token, client_stream_id=paused_stream_id)
        self.assertEqual((late.action, late.reason), ("deny", "connection_fenced"))
        self.assertIsNone(late.handle)
        self.assertEqual(f.process.registry.request_count, 2)
        self.assertEqual(f.process.registry.count, 1)

        f.process.finish_request(higher.handle)
        f.process.finish_request(f.request_handle)
        self.assertEqual(f.process.registry.request_count, 0)

    def test_close_exception_is_not_consumption_or_completion(self):
        f = self.fixture(
            full_lifecycle=True, delay_cancel=True, handler_type=FailingCloseHandler
        )
        f.establish()
        f.client_reader.feed_eof()
        f.sched.ready()
        self.assertTrue(f.item.task.done())
        self.assertFalse(f.item.close_consumed)
        self.assertEqual(f.exits, [87])
        self.assertEqual(
            f.process.acquire_request(f.item.token, client_stream_id=3).action, "deny"
        )
        self.assert_retained(f)

    def test_normal_full_lifecycle_releases_exact_token(self):
        f = self.fixture(full_lifecycle=True, upstream_tls=False)
        f.establish()
        f.handler.connected_gate.set_result(None)
        f.sched.ready()
        f.close()
        f.sched.ready()
        self.assertTrue(f.client_lifecycle.done())
        self.assertEqual(f.exits, [])
        self.assert_released(f)

    def test_terminal_observations_are_not_a_request_history(self):
        f = self.fixture(upstream_tls=False)
        f.establish()
        f.handler.connected_gate.set_result(None)
        f.sched.ready()
        f.close()
        f.sched.ready()
        self.assertEqual(len(f.process._h2_transports), 1)
        f.process.finish_request(f.request_handle)
        f.adapter.active_upstreams.clear()
        f.process.prune_h2_transports(f.adapter)
        self.assertFalse(f.process._h2_transports)
        self.assertEqual(f.process.registry.request_count, 0)
        f.process.client_disconnected(f.ctx.client)
        self.assert_released(f)

    def test_finished_request_idle_upstream_remains_owned_for_teardown(self):
        f = self.fixture(full_lifecycle=True, delay_cancel=True)
        f.establish()
        f.process.finish_request(f.request_handle)
        f.adapter.active_upstreams.clear()
        f.process.prune_h2_transports(f.adapter)
        self.assertEqual(len(f.process._h2_transports), 1)
        f.client_reader.feed_eof()
        f.sched.ready()
        self.assertTrue(f.item.close_consumed)
        self.assertEqual(f.exits, [86])
        self.assertIs(f.process.token_for(f.ctx.client), f.item.token)

    def test_background_sweep_skips_attached_h2_only(self):
        f = self.fixture()
        other = connection.Client(
            peername=("127.0.0.1", 1), sockname=("127.0.0.1", 443)
        )
        other_token = f.process.admit_tls("example.test").token
        self.assertTrue(f.process.track(other_token, other))
        with (
            mock.patch.object(
                f.process.registry,
                "expired_connections",
                return_value=(f.item.token, other_token),
            ),
            mock.patch.object(
                f.process, "_close_established", return_value=True
            ) as close,
        ):
            self.assertEqual(f.process.drain_sweep_once(), 1)
        close.assert_called_once_with(other)
        f.process.client_disconnected(other)

    def test_client_tls_normal_close_route(self):
        f = self.fixture()
        f.establish()
        f.adapter.wake_only = True
        f.sched.now(f.handler.server_event(events.DataReceived(f.ctx.client, b"")))
        wake = f.adapter.wakeup
        self.assertIs(f.root.command_sources[wake], f.adapter)
        tls = ClientTLSLayer(f.ctx.fork())
        tls.child_layer = f.root
        f.handler.layer = tls
        f.adapter.close_request = f.item
        f.sched.now(f.handler.server_event(events.Wakeup(wake)))
        self.assertTrue(f.item.close_consumed)
        f.sched.ready()
        self.assertEqual(f.exits, [86])

    def test_hard_exit_isolated_child_and_normal_control(self):
        parent = os.getpid()
        for mode, expected in (("fatal", 86), ("ownership", 87), ("normal", 0)):
            with self.subTest(mode=mode):
                result = subprocess.run(
                    [
                        sys.executable,
                        "-B",
                        str(Path(__file__).resolve()),
                        "--isolated-child",
                        mode,
                    ],
                    capture_output=True,
                    text=True,
                    timeout=10,
                    check=False,
                )
                self.assertEqual(
                    result.returncode, expected, (result.stdout, result.stderr)
                )
                self.assertIn("ISOLATED_CHILD_READY", result.stdout)
                self.assertEqual(os.getpid(), parent)
                self.assertNotIn("UNREACHABLE_AFTER_FATAL", result.stdout)

    def test_actual_system_rejects_unsupported_interpreter_without_retaining_tls(self):
        self._actual_system_assembly(connected=False, unsupported=True)

    def test_actual_system_adapter_runtime_assembly_disconnect(self):
        self._actual_system_assembly(connected=True)

    def test_actual_system_adapter_runtime_assembly_preconnect_cleanup(self):
        self._actual_system_assembly(connected=False)

    def test_actual_system_adapter_runtime_assembly_established_cleanup(self):
        self._actual_system_assembly(connected=True, normal_established=True)

    def test_actual_system_adapter_runtime_assembly_dial_error(self):
        self._actual_system_assembly(connected=False, connect_error="dial")

    def test_actual_system_adapter_runtime_assembly_connect_hook_error(self):
        self._actual_system_assembly(connected=False, connect_error="hook")

    def test_actual_system_adapter_runtime_assembly_tls_failure_retains_handle(self):
        self._actual_system_assembly(connected=True, transport_error=True)

    def _actual_system_assembly(
        self,
        *,
        connected,
        normal_established=False,
        connect_error=None,
        transport_error=False,
        unsupported=False,
    ):
        from credential_bound_h2 import BoundH2Server, dependency_versions_supported

        if not unsupported and not dependency_versions_supported():
            self.skipTest("supported CPython 3.10/3.11 pinned runtime assembly only")
        spec = importlib.util.spec_from_file_location(
            "h2_lifecycle_system", MITMSCRIPTS / "system.py"
        )
        system = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(system)
        scheduler = ReadyScheduler(
            FailingConnectLoop if connect_error == "dial" else MemoryConnectLoop
        )
        self.addCleanup(scheduler.close)
        proxyserver = Proxyserver()
        runtime = make_runtime(proxyserver, scheduler.loop)
        opts = options()
        context = Context(
            connection.Client(
                peername=("127.0.0.1", 50000),
                sockname=("127.0.0.1", 443),
                state=connection.ConnectionState.OPEN,
            ),
            opts,
        )
        context.client.alpn = b"h2"
        context.client.tls = True
        context.client.sni = "example.test"
        context.server.address = ("example.test", 443)
        context.server.tls = True
        admission = runtime.admit_tls("example.test")
        self.assertTrue(runtime.track(admission.token, context.client))
        addons = {"nextlayer": NextLayerSelector(), "proxyserver": proxyserver}
        master = SimpleNamespace(addons=SimpleNamespace(get=addons.get))
        patches = (
            mock.patch.object(ctx, "master", master, create=True),
            mock.patch.object(ctx, "options", opts, create=True),
            mock.patch.object(ctx, "log", mock.Mock(), create=True),
            mock.patch.object(system, "_live_runtime", runtime),
        )
        for patch in patches:
            patch.start()
            self.addCleanup(patch.stop)
        exits = []
        fatal = connected and not normal_established and not transport_error
        expected_exits = [86] if fatal else []
        application_completion_counts = []
        admitted_handles = []
        exit_patch = mock.patch.object(credential_bound.os, "_exit", exits.append)
        exit_patch.start()
        self.addCleanup(exit_patch.stop)

        class ActualSystemHandler(BaseHandler):
            async def handle_hook(self, hook):
                if isinstance(hook, layer.NextLayerHook):
                    system.next_layer(hook.data)
                elif isinstance(hook, server_hooks.ServerConnectHook):
                    system.server_connect(hook.data)
                    if connect_error == "hook":
                        hook.data.server.error = "synthetic addon connection refusal"
                    elif not connected and connect_error is None:
                        await self.connect_gate
                elif isinstance(hook, server_hooks.ServerConnectErrorHook):
                    system.server_connect_error(hook.data)
                elif isinstance(hook, server_hooks.ServerConnectedHook):
                    system.server_connected(hook.data)
                    self.hooks.append(hook.name)
                    try:
                        await self.connected_gate
                    except asyncio.CancelledError:
                        await self.client_gone
                        raise
                    return
                elif isinstance(hook, server_hooks.ServerDisconnectedHook):
                    system.server_disconnected(hook.data)
                    if transport_error:
                        await self.disconnected_gate
                elif isinstance(hook, TlsStartServerHook) and transport_error:
                    # Supply an ordinary memory-BIO TLS client through the
                    # addon hook. Verification stays on; no peer handshake or
                    # trusted certificate is fabricated by this failure test.
                    ssl_context = SSL.Context(SSL.TLS_CLIENT_METHOD)
                    ssl_context.set_verify(
                        SSL.VERIFY_PEER, lambda _c, _x, _e, _d, valid: valid
                    )
                    hook.data.ssl_conn = SSL.Connection(ssl_context, None)
                    hook.data.ssl_conn.set_connect_state()
                elif isinstance(hook, TlsStartServerHook) and normal_established:
                    self.hooks.append(hook.name)
                    await self.tls_gate
                    return
                elif isinstance(hook, server_hooks.ClientDisconnectedHook):
                    self.wakes_cancelled = not self.wakeup_timer
                    system.client_disconnected(hook.client)
                    self.client_gone.set_result(None)
                elif isinstance(
                    hook,
                    (
                        layers.HttpRequestHeadersHook,
                        layers.HttpRequestHook,
                        layers.HttpResponseHeadersHook,
                        layers.HttpResponseHook,
                        layers.HttpErrorHook,
                    ),
                ):
                    getattr(system, hook.name)(hook.flow)
                    if isinstance(hook, layers.HttpRequestHeadersHook):
                        admitted_handles.append(
                            hook.flow.metadata[system.FLOW_REQUEST_HANDLE_KEY]
                        )
                    if isinstance(
                        hook, (layers.HttpResponseHook, layers.HttpErrorHook)
                    ):
                        application_completion_counts.append(
                            runtime.registry.request_count
                        )
                await super().handle_hook(hook)

        handler = ActualSystemHandler(context, None)
        handler.layer = TransparentProxy(context)
        handler.connected_gate = scheduler.loop.create_future()
        handler.connect_gate = scheduler.loop.create_future()
        handler.tls_gate = scheduler.loop.create_future()
        handler.disconnected_gate = scheduler.loop.create_future()
        handler.client_gone = scheduler.loop.create_future()
        peer = h2.connection.H2Connection(h2.config.H2Configuration(client_side=True))
        peer.initiate_connection()
        reader = asyncio.StreamReader(loop=scheduler.loop)
        handler.transports[context.client] = server.ConnectionIO(
            reader=reader, writer=MemoryClientWriter(peer)
        )
        deregistered = []

        async def registered_client():
            with proxyserver.register_connection(context.client.id, handler):
                await handler.handle_client()
                self.assertEqual(exits, expected_exits)
            deregistered.append(True)

        if unsupported:
            version_patch = mock.patch("credential_bound_h2.sys.version_info", (3, 12, 0))
            version_patch.start()
            self.addCleanup(version_patch.stop)
        lifecycle = scheduler.loop.create_task(registered_client())
        scheduler.ready()
        reader.feed_data(peer.data_to_send())
        scheduler.ready()
        adapter = runtime.h2_for(context.client)
        if unsupported:
            self.assertIsNone(adapter)
            self.assertTrue(lifecycle.done())
            self.assertTrue(deregistered)
            self.assertNotIn("server_connect", handler.hooks)
            self.assertEqual(runtime.registry.count, 0)
            self.assertEqual(runtime.registry.request_count, 0)
            self.assertEqual(exits, [])
            return
        self.assertIs(
            type(adapter),
            BoundH2Server,
            (ctx.log.mock_calls, handler.hooks, handler.layer),
        )
        self.assertIs(type(adapter.http_layer), layers.HttpLayer)
        self.assertIs(adapter.runtime, runtime)
        self.assertIs(adapter.token, admission.token)
        peer.send_headers(
            1,
            [
                (":method", "POST"),
                (":scheme", "https"),
                (":authority", "example.test"),
                (":path", "/1"),
            ],
            end_stream=connect_error is not None or transport_error,
        )
        reader.feed_data(peer.data_to_send())
        scheduler.ready()
        self.assertFalse(adapter.failed)
        flow = handler.flows[1]
        self.assertEqual(len(admitted_handles), 1)
        handle = admitted_handles[0]
        self.assertEqual(handle.client_stream_id, 1)
        self.assertEqual(flow.request.headers["Authorization"], "synthetic-test-value")
        self.assertEqual(
            runtime.registry.drain_state(admission.token).max_admitted_stream_id, 1
        )
        if connect_error is not None:
            self.assertEqual(exits, [])
            self.assertFalse(lifecycle.done())
            self.assertIs(runtime.token_for(context.client), admission.token)
            # Deliver the adapter's registered timer through the real dispatcher.
            if adapter._wakeup is not None:
                scheduler.now(handler.server_event(events.Wakeup(adapter._wakeup)))
                scheduler.ready()
            self.assertEqual(runtime.registry.request_count, 0)
            self.assertFalse(runtime._h2_transports)
            self.assertEqual(runtime.registry.count, 1)
            self.assertIs(runtime.h2_for(context.client), adapter)
            reader.feed_eof()
            scheduler.ready()
            self.assertTrue(lifecycle.done())
            self.assertTrue(deregistered)
            self.assertIsNone(runtime.token_for(context.client))
            self.assertIsNone(runtime.h2_for(context.client))
            self.assertEqual(runtime.registry.count, 0)
            self.assertEqual(exits, [])
            return
        self.assertEqual(runtime.registry.request_count, 1)
        self.assertEqual(len(runtime._h2_transports), 1)
        observation = next(iter(runtime._h2_transports.values()))
        writer = handler.transports[observation.connection].writer
        if writer is not None:
            self.addCleanup(
                writer.close
            )  # fixture cleanup while scheduler still exists
        if normal_established or transport_error:
            # The ordinary task now enters handle_connection, while an addon
            # holds the separate TLS-start hook. No handshake is simulated.
            handler.connected_gate.set_result(None)
            scheduler.ready()
            self.assertIn("tls_start_server", handler.hooks)
            self.assertFalse(observation.task.done())
            self.assertFalse(writer.is_closing())
        if transport_error:
            record = adapter._active[1]
            self.assertIs(record.upstream[0], observation.connection)
            self.assertIsNot(flow.server_conn, observation.connection)
            handler.transports[observation.connection].reader.feed_data(
                b"invalid TLS record bytes"
            )
            scheduler.ready()
            self.assertTrue(writer.is_closing())
            self.assertNotIn(observation.connection, handler.transports)
            self.assertTrue(observation.disconnected_hook_seen)
            self.assertFalse(observation.task.done())
            self.assertTrue(application_completion_counts)
            self.assertTrue(all(count == 1 for count in application_completion_counts))
            self.assertEqual(runtime.registry.request_count, 1)
            self.assertIs(record.upstream[0], observation.connection)
            self.assertIsNot(flow.server_conn, observation.connection)
            if adapter._wakeup is not None:
                scheduler.now(handler.server_event(events.Wakeup(adapter._wakeup)))
                scheduler.ready()
            self.assertEqual(runtime.registry.request_count, 1)
            self.assertFalse(observation.terminal)
            self.assertEqual(exits, [])
            handler.disconnected_gate.set_result(None)
            scheduler.ready()
            self.assertTrue(observation.task.done())
            if adapter._wakeup is not None:
                scheduler.now(handler.server_event(events.Wakeup(adapter._wakeup)))
                scheduler.ready()
            self.assertEqual(runtime.registry.request_count, 0)
            self.assertFalse(runtime._h2_transports)
            self.assertIs(runtime.token_for(context.client), admission.token)
            reader.feed_eof()
            scheduler.ready()
            self.assertTrue(lifecycle.done())
            self.assertTrue(deregistered)
            self.assertIsNone(runtime.token_for(context.client))
            self.assertEqual(runtime.registry.count, 0)
            self.assertEqual(exits, [])
            return
        self.assertFalse(observation.close_consumed)
        # Explicit application-hook slice: neither hook establishes that the
        # pending upstream upload or downstream protocol obligations are gone.
        flow.response = http.Response.make(200, b"")
        scheduler.now(handler.handle_hook(layers.HttpResponseHook(flow)))
        scheduler.now(handler.handle_hook(layers.HttpErrorHook(flow)))
        self.assertEqual(application_completion_counts, [1, 1])
        self.assertIs(flow.metadata[system.FLOW_REQUEST_HANDLE_KEY], handle)
        self.assertIs(runtime.token_for(context.client), admission.token)
        reader.feed_eof()
        scheduler.ready()
        self.assertTrue(lifecycle.done())
        self.assertTrue(deregistered)
        self.assertTrue(handler.wakes_cancelled)
        self.assertEqual(exits, expected_exits)
        self.assertTrue(observation.close_consumed)
        self.assertEqual(application_completion_counts, [1, 1])
        if fatal:
            self.assertFalse(writer.is_closing())
            self.assertIs(runtime.token_for(context.client), admission.token)
            self.assertEqual(runtime.registry.request_count, 1)
        else:
            if normal_established:
                self.assertTrue(writer.is_closing())
                self.assertNotIn(observation.connection, handler.transports)
                self.assertTrue(observation.disconnected_hook_seen)
            else:
                self.assertIsNone(writer)
            self.assertIsNone(runtime.token_for(context.client))
            self.assertIsNone(runtime.h2_for(context.client))
            self.assertEqual(runtime.registry.request_count, 0)
            self.assertEqual(runtime.registry.count, 0)
            self.assertFalse(runtime._h2_transports)
        self.assertEqual(
            runtime.acquire_request(admission.token, client_stream_id=3).action, "deny"
        )


def isolated_child(mode):
    assert sys.argv[1:] == ["--isolated-child", mode]
    f = Fixture(upstream_tls=mode != "normal", real_exit=True)
    try:
        f.establish()
        if mode == "normal":
            f.handler.connected_gate.set_result(None)
            f.sched.ready()
        print("ISOLATED_CHILD_READY", flush=True)
        if mode == "ownership":
            f.process._block_h2_cleanup(f.adapter)
            raise AssertionError("UNREACHABLE_AFTER_FATAL")
        f.close()
        f.sched.ready()
        if mode == "fatal":
            raise AssertionError("UNREACHABLE_AFTER_FATAL")
        assert not f.process._h2_exit_started
    finally:
        f.cleanup()


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--isolated-child":
        if sys.argv[2] not in ("fatal", "ownership", "normal"):
            raise SystemExit(2)
        isolated_child(sys.argv[2])
    else:
        unittest.main()
