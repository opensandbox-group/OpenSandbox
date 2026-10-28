# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0
"""Real pinned handler/watchdog scheduling, memory transports, no sockets."""
import asyncio
import heapq
import hashlib
import json
import unittest
from unittest import mock

from test_credential_bound_h2_lifecycle import (
    ReadyScheduler, BaseHandler, MemoryClientWriter, make_runtime, options,
)
from credential_bound_h2 import BoundH2Server, dependency_versions_supported
from revision_receiver import Revision
from mitmproxy import connection, http
from mitmproxy.addons.proxyserver import Proxyserver
from mitmproxy.proxy import commands, events, server, server_hooks
from mitmproxy.proxy.context import Context
from mitmproxy.proxy.layers import http as layers
import h2.config
import h2.connection


@unittest.skipUnless(dependency_versions_supported(), "supported CPython 3.10/3.11 pinned runtime only")
class WakeupTests(unittest.TestCase):
    def setUp(self):
        self.now = 1000.0
        self.clock = mock.patch('time.time', lambda: self.now)
        self.clock.start()
        self.addCleanup(self.clock.stop)
        self.sched = ReadyScheduler()
        self.addCleanup(self.sched.close)
        self.sched.loop.time = lambda: self.now
        self.proxy = Proxyserver()
        self.runtime = make_runtime(self.proxy, self.sched.loop)
        self.exits = []
        self.exit_patch = mock.patch('credential_bound.os._exit', self.exits.append)
        self.exit_patch.start()
        self.addCleanup(self.exit_patch.stop)
        context = Context(connection.Client(peername=('127.0.0.1', 1),
            sockname=('127.0.0.1', 443), state=connection.ConnectionState.OPEN), options())
        context.client.alpn = b'h2'
        context.client.tls = True
        context.client.sni = 'example.test'
        context.server.address = ('example.test', 443)
        context.server.tls = True
        token = self.runtime.admit_tls('example.test').token
        self.runtime.track(token, context.client)
        self.root = layers.HttpLayer(context, layers.HTTPMode.transparent)
        self.adapter = BoundH2Server(context.fork(), self.runtime, self.root, token)
        test = self

        class Handler(BaseHandler):
            async def handle_hook(self, hook):
                if isinstance(hook, layers.HttpRequestHeadersHook):
                    flow = hook.flow
                    identity = test.adapter.consume_identity(flow.request)
                    admission = test.runtime.acquire_request(token, client_stream_id=identity.stream_id)
                    test.adapter.bind_request(identity, flow, admission.handle)
                    # Buffer incomplete uploads, with no upstream connection or
                    # suspended addon hook. The ordinary watchdog remains armed.
                    if flow.request.path == '/local':
                        flow.response = http.Response.make(200, b'ok')
                elif isinstance(hook, (layers.HttpResponseHook, layers.HttpErrorHook)):
                    test.adapter.application_done(hook.flow)
                elif isinstance(hook, server_hooks.ClientDisconnectedHook):
                    test.runtime.client_disconnected(context.client)

        self.handler = Handler(context, None)
        self.handler.layer = self.root
        self.original_wakeup = self.handler.wakeup
        self.handler.timeout_watchdog.timeout = 0.35
        self.root.connections[context.client] = self.adapter
        self.registration = self.proxy.register_connection(context.client.id, self.handler)
        self.registration.__enter__()
        self.addCleanup(lambda: self.registration.__exit__(None, None, None))
        self.runtime.attach_h2(context.client, self.adapter)
        self.peer = h2.connection.H2Connection(h2.config.H2Configuration(client_side=True))
        self.peer.initiate_connection()
        self.reader = asyncio.StreamReader(loop=self.sched.loop)
        self.handler.transports[context.client] = server.ConnectionIO(
            reader=self.reader, writer=MemoryClientWriter(self.peer))
        self.lifecycle = self.sched.loop.create_task(self.handler.handle_client())
        self.sched.ready()
        self.reader.feed_data(self.peer.data_to_send())
        self.sched.ready()
        self.wakeup = self.adapter._wakeup
        self.assertIs(self.root.command_sources[self.wakeup], self.adapter)

    def advance(self, seconds=.11):
        self.now += seconds
        while self.sched.loop._scheduled and self.sched.loop._scheduled[0]._when <= self.now:
            timer = heapq.heappop(self.sched.loop._scheduled)
            if not timer.cancelled():
                timer._run()
                self.sched.ready()
        self.sched.ready()

    def request(self, path, end):
        self.peer.send_headers(1, [(':method', 'POST'), (':scheme', 'https'),
            (':authority', 'example.test'), (':path', path)], end_stream=end)
        self.reader.feed_data(self.peer.data_to_send())
        self.sched.ready()

    def assert_timeout_and_release(self):
        before = self.handler.timeout_watchdog.last_activity
        self.advance(); self.advance()
        self.assertEqual(self.handler.timeout_watchdog.last_activity, before)
        self.assertEqual(len(self.handler.wakeup_timer), 1)
        self.advance(); self.advance()
        self.assertTrue(self.lifecycle.done())
        self.assertFalse(self.handler.wakeup_timer)
        self.assertFalse(self.runtime._h2_wakeup_hooks)
        self.assertEqual(self.handler.wakeup, self.original_wakeup)
        self.assertEqual(self.runtime.registry.count, 0)
        self.assertEqual(self.runtime.registry.request_count, 0)
        self.assertEqual(self.exits, [])

    def test_idle_handler_times_out_without_housekeeping_activity(self):
        self.assert_timeout_and_release()

    def test_stalled_upload_times_out_and_releases_its_handle(self):
        self.request('/stalled', False)
        self.assertEqual(self.runtime.registry.request_count, 1)
        self.assertFalse(self.adapter.maintenance_needed())
        self.assert_timeout_and_release()

    def test_completed_local_response_then_idle_times_out(self):
        self.request('/local', True)
        self.advance()
        self.assertEqual(self.runtime.registry.request_count, 0)
        self.assert_timeout_and_release()

    def test_shutdown_fence_wakes_without_client_traffic(self):
        self.runtime._receiver.close()
        self.advance()
        self.assertTrue(self.lifecycle.done())
        self.assertNotIn(self.wakeup, self.root.command_sources)
        self.assertFalse(self.handler.wakeup_timer)
        self.assertEqual(self.runtime.registry.count, 0)
        self.assertEqual(self.exits, [])

    def retire_host(self):
        payload = json.dumps({"version": 1, "vaultRevision": 2,
            "effectivePolicyEpoch": 0, "interceptionMode": "credential-bound",
            "state": "active-empty", "tlsBindingHostSelectors": [],
            "fullRenderedBindings": [], "redactions": []}).encode()
        revision = Revision("control", "subject", 2, 2, 0,
                            hashlib.sha256(payload).hexdigest())
        self.runtime._receiver.prepare(revision, payload)
        self.runtime._receiver.commit(revision)

    def test_retirement_of_idle_connection_dispatches_exact_wakeup_once(self):
        self.retire_host()
        self.advance()
        self.assertTrue(self.adapter.goaway_sent)
        self.assertNotIn(self.wakeup, self.root.command_sources)
        self.assertTrue(self.lifecycle.done())
        self.assertFalse(self.handler.wakeup_timer)
        self.assertEqual(self.runtime.registry.count, 0)

    def test_future_retirement_with_held_stream_does_not_refresh_watchdog(self):
        self.request('/stalled', False)
        self.retire_host()
        state = self.runtime.registry.drain_state(self.adapter.token)
        self.assertTrue(state.fenced)
        self.assertIsNotNone(state.deadline)
        self.assertTrue(self.adapter.maintenance_needed())
        first_deadline = state.deadline
        self.advance()
        self.assertTrue(self.adapter.retirement_notice_sent)
        self.assertFalse(self.adapter.goaway_sent)
        self.assertEqual(self.runtime.registry.drain_state(self.adapter.token).deadline,
                         first_deadline)
        self.assertEqual(self.runtime.registry.request_count, 1)
        self.assert_timeout_and_release()

    def test_retirement_deadline_closes_held_stream_without_network(self):
        self.request('/stalled', False)
        self.retire_host()
        deadline = self.runtime.registry.drain_state(self.adapter.token).deadline
        # Only the published drain clock advances; watchdog remains before its
        # inactivity deadline. No client event drives this terminal transition.
        with mock.patch('credential_bound_h2.time.monotonic', return_value=deadline + 1):
            self.advance()
        self.assertTrue(self.lifecycle.done())
        self.assertEqual(self.runtime.registry.request_count, 0)
        self.assertEqual(self.runtime.registry.count, 0)
        self.assertFalse(self.handler.wakeup_timer)
        self.assertEqual(self.exits, [])

    def test_disarmed_hook_blocks_timeout_until_resume(self):
        before = self.handler.timeout_watchdog.last_activity
        # Put the real watchdog at its disarm gate (the dependency does not
        # interrupt an already-running sleep when a new hook starts).
        old_watch = next(task for task in asyncio.all_tasks(self.sched.loop)
                         if task.get_coro().__qualname__ == "TimeoutWatchdog.watch")
        old_watch.cancel()
        self.sched.ready()
        with self.handler.timeout_watchdog.disarm():
            watch = self.sched.loop.create_task(self.handler.timeout_watchdog.watch())
            self.sched.ready()
            self.advance(.5)
            self.advance(.5)
            self.assertFalse(self.lifecycle.done())
            self.assertEqual(self.handler.timeout_watchdog.last_activity, before)
        self.assertEqual(self.handler.timeout_watchdog.last_activity, self.now)
        self.sched.ready()
        self.assert_timeout_and_release()
        self.assertTrue(watch.done())

    def test_cancel_sleeping_maintenance_cleans_task_and_restores_callback(self):
        self.reader.feed_eof()
        self.sched.ready()
        self.assertTrue(self.lifecycle.done())
        self.assertFalse(self.handler.wakeup_timer)
        self.assertFalse(self.runtime._h2_wakeup_hooks)
        self.assertEqual(self.handler.wakeup, self.original_wakeup)
        self.assertEqual(self.runtime.registry.count, 0)

    def test_current_admission_with_changed_handler_fails_closed(self):
        self.proxy.connections[self.handler.client.id] = BaseHandler(self.root.context, None)
        self.advance()
        self.assertEqual(self.exits, [87])
        self.assertEqual(self.runtime.registry.count, 1)
        self.assertFalse(self.handler.wakeup_timer)

    def test_foreign_wakeup_uses_original_dispatch(self):
        other = commands.RequestWakeup(.1)
        seen = []
        async def capture(event):
            seen.append(event)
        with mock.patch.object(self.handler, 'server_event', capture):
            task = self.sched.loop.create_task(self.handler.wakeup(other))
            self.handler.wakeup_timer.add(task)
            self.sched.ready()
            self.advance()
        self.assertEqual([event.command for event in seen], [other])
        self.assertTrue(task.done())
        self.assertNotIn(task, self.handler.wakeup_timer)
        self.assertIs(self.root.command_sources[self.wakeup], self.adapter)

    def test_predicate_failure_exits_once_without_releasing(self):
        with mock.patch.object(self.adapter, 'maintenance_needed', side_effect=ValueError):
            self.advance()
        self.assertEqual(self.exits, [87])
        self.runtime._block_h2_cleanup(self.adapter)
        self.assertEqual(self.exits, [87])
        self.assertEqual(self.runtime.registry.count, 1)

    def test_replaced_wrapper_is_not_overwritten_on_restore(self):
        replacement = mock.AsyncMock()
        self.handler.wakeup = replacement
        self.runtime._restore_h2_wakeups(self.handler.client)
        self.sched.ready()
        self.assertIs(self.handler.wakeup, replacement)
        self.assertFalse(self.handler.wakeup_timer)
        self.assertFalse(self.runtime._h2_wakeup_hooks)


if __name__ == '__main__':
    unittest.main()
