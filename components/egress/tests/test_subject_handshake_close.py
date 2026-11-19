# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Fast Sandbox deny-handshake closure: exact-client close and dispatch.

Two scopes, both with the real subject admission runtime and the real
system addon code under test:

- ``SubjectHandshakeCloseTest`` drives the real ``SubjectAdmissionRuntime``
  against fake handlers/loops: the close targets one exact client, never
  holds the runtime lock, never waits for disconnect, and stays fail-closed
  (state fence, honest "not closed" result) when the handler, loop or close
  itself fails.
- ``SubjectDenyDispatchTest`` drives ``system.tls_clienthello`` dispatch:
  every subject deny (pre-admission, admission result, and hook exception)
  reaches the Fast Sandbox close entry, while the sidecar branch keeps the
  shared ``credential_bound.terminate_handshake`` call with the same
  arguments and call count.
"""

import asyncio
import sys
import types
import unittest
from unittest import mock

import test_credential_bound as fixtures
import test_subject_revision as f


class FakeClient:
    def __init__(self, address="192.0.2.1"):
        self.peername = (address, 1234)
        self.id = object()
        self.state = "open"
        self.closed = False


class RecordingHandler:
    """Real-handler stand-in: records closes, identity and lock state."""

    def __init__(self, client, runtime=None):
        self.client = client
        self.closed = []
        self.locked_at_close = None
        self.fail = False
        self._runtime = runtime

    def close_connection(self, client):
        self.locked_at_close = bool(self._runtime and self._runtime._lock.locked())
        if self.fail:
            raise RuntimeError("close failed")
        self.closed.append(client)


class QueueingLoop:
    """Off-loop stand-in: queues callbacks; can refuse like a closed loop."""

    def __init__(self):
        self.queued = []
        self.refuse = False

    def call_soon_threadsafe(self, callback, *args):
        if self.refuse:
            raise RuntimeError("event loop is closed")
        self.queued.append((callback, args))

    def flush(self):
        pending, self.queued = self.queued, []
        for callback, args in pending:
            callback(*args)


class SubjectHandshakeCloseTest(unittest.TestCase):
    def setUp(self):
        self.owner = f.make_owner(tls_capacity=2, process_tls_capacity=3, static_passthrough=())
        self.addCleanup(self.owner.close)
        self.sequence = 0
        self.sequence += 1
        self.owner.lifecycle("register", f.command(self.sequence, sourceAddresses=["192.0.2.1"]))
        from subject_admission import SubjectAdmissionRuntime

        self.runtime = SubjectAdmissionRuntime(self.owner)
        self.loop = asyncio.new_event_loop()
        self.addCleanup(self.loop.close)
        self.proxyserver = types.SimpleNamespace(connections={})
        self.runtime._transport = (self.loop, self.proxyserver)

    def install(self, epoch=1, host="api.example.com"):
        item = f.snapshot(f.A, epoch, host)
        self.owner.prepare(f.A, item.revision, item.payload)
        self.owner.commit(f.A, item.revision)
        return item

    def connect(self, client, sni="api.example.com"):
        self.runtime.client_connected(client)
        self.proxyserver.connections[client.id] = RecordingHandler(client, self.runtime)
        return self.runtime.admit_tls(client, sni, destination_port=443)

    def deny_inline(self, client):
        async def terminate():
            return self.runtime.terminate_handshake(client)
        return self.loop.run_until_complete(terminate())

    def registry(self):
        return self.owner.lookup_source("192.0.2.1").registry

    def test_inline_close_is_exact_and_spares_same_source_peer(self):
        self.install()
        first, second = FakeClient(), FakeClient()
        self.connect(first)
        result = self.connect(second)
        self.assertEqual(result.action, "decrypt")
        self.assertTrue(self.deny_inline(first))
        handler_first = self.proxyserver.connections[first.id]
        handler_second = self.proxyserver.connections[second.id]
        self.assertEqual(handler_first.closed, [first])
        self.assertEqual(handler_second.closed, [])
        self.assertFalse(handler_first.locked_at_close, "close ran while the runtime lock was held")
        member = self.runtime._clients[id(first)]
        self.assertTrue(member.doomed)
        self.assertFalse(self.runtime._clients[id(second)].doomed)

    def test_close_neither_waits_for_disconnect_nor_releases_the_token(self):
        self.install()
        client = FakeClient()
        admitted = self.connect(client)
        token = admitted.token
        self.assertTrue(self.deny_inline(client))
        # No synchronous disconnect happened: the member and the budget
        # stay until the real client_disconnected hook fires.
        self.assertIn(id(client), self.runtime._clients)
        self.assertEqual(self.registry().count, 1)
        self.assertIs(self.runtime.token_for(client), token)
        self.assertEqual(self.runtime.acquire_request(token).reason, "connection_fenced")
        self.runtime.client_disconnected(client)
        self.assertEqual(self.registry().count, 0)
        self.assertNotIn(id(client), self.runtime._clients)

    def test_denied_client_without_token_leaks_no_membership(self):
        client = FakeClient()  # registered source, no snapshot installed
        self.assertEqual(self.connect(client).reason, "snapshot_missing")
        self.assertTrue(self.deny_inline(client))
        self.assertEqual(self.registry().count, 0)
        self.assertEqual(self.runtime._tokens, {})
        self.assertEqual(self.runtime._connections, {})
        self.assertTrue(self.runtime._clients[id(client)].doomed)
        self.runtime.client_disconnected(client)
        self.assertNotIn(id(client), self.runtime._clients)
        self.assertEqual(self.registry().budget_observations()[0], ("process", 0, 3))

    def test_delayed_callback_rechecks_replaced_and_missing_handler(self):
        self.install()
        client = FakeClient()
        self.connect(client)
        original = self.proxyserver.connections[client.id]
        queue = QueueingLoop()
        self.runtime._transport = (queue, self.proxyserver)
        # Scheduling from off the loop is not a confirmed close.
        self.assertFalse(self.runtime.terminate_handshake(client))
        self.assertTrue(client.closed, "fail-closed state fence missing")
        replacement = RecordingHandler(FakeClient(), self.runtime)
        self.proxyserver.connections[client.id] = replacement
        queue.flush()
        self.assertEqual(replacement.closed, [])
        self.assertEqual(original.closed, [])
        self.assertFalse(self.runtime.terminate_handshake(client))
        self.assertEqual(len(queue.queued), 1)
        del self.proxyserver.connections[client.id]
        queue.flush()
        self.assertEqual(queue.queued, [])
        self.assertEqual(replacement.closed, [])
        self.assertEqual(original.closed, [])

    def test_unindexed_client_closes_without_creating_membership(self):
        client = FakeClient()
        handler = RecordingHandler(client, self.runtime)
        self.proxyserver.connections[client.id] = handler
        self.assertNotIn(id(client), self.runtime._clients)
        self.assertTrue(self.deny_inline(client))
        self.assertEqual(handler.closed, [client])
        self.assertFalse(handler.locked_at_close)
        self.assertEqual(self.runtime._clients, {})
        self.assertEqual(self.runtime._tokens, {})
        self.assertEqual(self.registry().count, 0)

    def test_repeated_terminate_is_safe_and_stays_exact(self):
        self.install()
        client, other = FakeClient(), FakeClient()
        self.connect(client)
        self.connect(other)
        self.assertTrue(self.deny_inline(client))
        self.assertTrue(self.deny_inline(client))
        handler = self.proxyserver.connections[client.id]
        self.assertEqual(handler.closed, [client, client])
        self.assertEqual(self.proxyserver.connections[other.id].closed, [])
        self.runtime.client_disconnected(client)

    def test_missing_handler_loop_and_close_failure_stay_fail_closed(self):
        for mode in ("no-handler", "close-fails", "no-loop", "closed-loop"):
            with self.subTest(mode=mode):
                self.install()
                client = FakeClient()
                self.connect(client)
                if mode == "no-handler":
                    del self.proxyserver.connections[client.id]
                    self.assertFalse(self.deny_inline(client))
                elif mode == "close-fails":
                    self.proxyserver.connections[client.id].fail = True
                    self.assertFalse(self.deny_inline(client))
                elif mode == "no-loop":
                    self.runtime._transport = (None, None)
                    self.assertFalse(self.runtime.terminate_handshake(client))
                else:
                    queue = QueueingLoop()
                    queue.refuse = True
                    self.runtime._transport = (queue, self.proxyserver)
                    self.assertFalse(self.runtime.terminate_handshake(client))
                self.assertTrue(client.closed, "fail-closed state fence missing")
                self.assertTrue(self.runtime._clients[id(client)].doomed)
                self.assertEqual(self.registry().count, 1)  # nothing was released
                self.runtime.client_disconnected(client)
        # "no-handler"/"close-fails" ran inline on the real loop; closing the
        # loop afterwards must not turn the queued branch into an error.
        self.loop.run_until_complete(asyncio.sleep(0))

    def test_doomed_member_never_reacquires_authority(self):
        self.install()
        client = FakeClient()
        admitted = self.connect(client)
        self.assertTrue(self.deny_inline(client))
        # A renegotiation attempt on the denied connection cannot adopt the
        # source owner again, and no live token is replaced or released.
        self.assertEqual(self.runtime.admit_tls(client, "api.example.com",
                                                destination_port=443).reason,
                         "unknown_identity")
        self.assertIs(self.runtime.token_for(client), admitted.token)
        self.assertEqual(self.registry().count, 1)


class SubjectDenyDispatchTest(unittest.TestCase):
    def setUp(self):
        fixtures._isolate_system_modules(self)
        sys.path.insert(0, str(fixtures.MITMSCRIPTS))
        self.addCleanup(sys.path.remove, str(fixtures.MITMSCRIPTS))
        self.system, self.receiver, self.runtime = fixtures._system_with_live_runtime()
        self.system._live_runtime = None
        # The subject module must not leak in from other test files, and the
        # fake installed below must not leak out: subject_revision lazily
        # imports the real one inside register.
        previous = sys.modules.pop("subject_admission", None)
        if previous is None:
            self.addCleanup(sys.modules.pop, "subject_admission", None)
        else:
            self.addCleanup(sys.modules.__setitem__, "subject_admission", previous)

    def subject_runtime(self, action="deny", reason="snapshot_missing"):
        subject = mock.Mock()
        subject.admit_tls.return_value = types.SimpleNamespace(action=action, reason=reason, token=None)
        module = types.ModuleType("subject_admission")
        module.runtime = lambda: subject
        sys.modules["subject_admission"] = module
        return subject

    def deny_events(self):
        prefix = "credential proxy: tls-admission-v1 "
        return [message for message in self.system.ctx.log.messages if message.startswith(prefix)]

    def shared_close(self):
        import credential_bound

        patcher = mock.patch.object(credential_bound, "terminate_handshake")
        shared = patcher.start()
        self.addCleanup(patcher.stop)
        return shared

    def test_subject_admission_denies_reach_the_runtime_close_entry(self):
        for reason in ("snapshot_missing", "unknown_identity", "registry_exhausted",
                       "generation_mismatch"):
            with self.subTest(reason=reason):
                self.system.ctx.log.messages.clear()
                subject = self.subject_runtime(reason=reason)
                shared = self.shared_close()
                data = fixtures._ClientHelloData("api.example.com")
                self.system.tls_clienthello(data)
                subject.admit_tls.assert_called_once()
                self.assertIs(subject.admit_tls.call_args[0][0], data.client_conn)
                subject.terminate_handshake.assert_called_once_with(data.client_conn)
                shared.assert_not_called()
                self.assertFalse(data.ignore_connection)
                self.assertEqual(self.deny_events(),
                                 ["credential proxy: tls-admission-v1 deny " + reason])

    def test_subject_pre_admission_denies_reach_the_runtime_close_entry(self):
        for reason, configure in (
                ("ssl_insecure", lambda data: setattr(self.system.ctx.options, "ssl_insecure", True)),
                ("malformed_extensions", lambda data: setattr(data.client_hello, "extensions", None))):
            with self.subTest(reason=reason):
                self.system.ctx.log.messages.clear()
                self.system.ctx.options.ssl_insecure = False
                subject = self.subject_runtime()
                shared = self.shared_close()
                data = fixtures._ClientHelloData("api.example.com")
                configure(data)
                self.system.tls_clienthello(data)
                subject.admit_tls.assert_not_called()
                subject.terminate_handshake.assert_called_once_with(data.client_conn)
                shared.assert_not_called()
                self.assertEqual(self.deny_events(),
                                 ["credential proxy: tls-admission-v1 deny " + reason])

    def test_subject_hook_exception_reaches_the_runtime_close_entry(self):
        subject = self.subject_runtime()
        subject.admit_tls.side_effect = RuntimeError("admission failure")
        shared = self.shared_close()
        data = fixtures._ClientHelloData("api.example.com")
        self.system.tls_clienthello(data)  # must not raise into mitmproxy
        subject.terminate_handshake.assert_called_once_with(data.client_conn)
        shared.assert_not_called()
        self.assertEqual(self.deny_events(),
                         ["credential proxy: tls-admission-v1 deny invalid_input"])

    def test_subject_decrypt_and_passthrough_never_close(self):
        for action, reason in (("decrypt", "binding_host"), ("passthrough", "no_binding_host")):
            with self.subTest(action=action):
                subject = self.subject_runtime(action=action, reason=reason)
                shared = self.shared_close()
                data = fixtures._ClientHelloData("api.example.com")
                self.system.tls_clienthello(data)
                subject.terminate_handshake.assert_not_called()
                shared.assert_not_called()
                self.assertEqual(data.ignore_connection, action == "passthrough")

    def test_sidecar_denies_keep_the_shared_close_entry(self):
        for reason in ("snapshot_missing", "ssl_insecure", "malformed_extensions"):
            with self.subTest(reason=reason):
                self.system.ctx.log.messages.clear()
                self.system.ctx.options.ssl_insecure = False
                shared = self.shared_close()
                live = mock.Mock()
                live.admit_tls.return_value = types.SimpleNamespace(
                    action="deny", reason=reason, token=None)
                self.system._live_runtime = live
                data = fixtures._ClientHelloData("api.example.com")
                if reason == "ssl_insecure":
                    self.system.ctx.options.ssl_insecure = True
                elif reason == "malformed_extensions":
                    data.client_hello.extensions = None
                self.system.tls_clienthello(data)
                shared.assert_called_once_with(data.client_conn)
                live.terminate_handshake.assert_not_called()
                self.assertFalse(data.ignore_connection)

    def test_off_branch_keeps_legacy_behavior(self):
        data = fixtures._ClientHelloData("api.example.com")
        shared = self.shared_close()
        self.system.tls_clienthello(data)
        shared.assert_not_called()
        self.assertFalse(data.ignore_connection)
        self.assertFalse(data.client_conn.closed)


if __name__ == "__main__":
    unittest.main()
