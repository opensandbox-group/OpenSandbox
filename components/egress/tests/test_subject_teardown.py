# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Lifecycle teardown of proxied subject clients, under event-controlled races."""

import asyncio
import threading
import types
import unittest
from unittest.mock import patch

import test_subject_revision as f  # noqa: I001 - installs the mitmscripts path
import subject_admission
from subject_admission import SubjectAdmissionRuntime

HOST = "api.example.com"
C = f.subject.Target("s-c", "generation-c")


class Handler:
    """mitmproxy handler stand-in; "disconnect" fires the real disconnect hook."""

    def __init__(self, test):
        self.test, self.mode, self.closed, self.hook = test, "disconnect", [], None

    def close_connection(self, client):
        # Closure IO never runs under an owner or runtime lock.
        self.test.assertFalse(self.test.owner._operation_lock.locked())
        self.test.assertFalse(self.test.runtime._lock.locked())
        self.closed.append(client)
        if self.hook is not None:
            self.hook(client)
        if self.mode == "raise":
            raise RuntimeError("close failed")
        if self.mode == "disconnect":
            self.test.runtime.client_disconnected(client)


class InlineLoop:
    def call_soon_threadsafe(self, callback, *args):
        callback(*args)


class TeardownTest(unittest.TestCase):
    def setUp(self):
        self.owner = f.make_owner(max_subjects=3, retained_payload_budget=12288, tls_capacity=4,
                                  process_tls_capacity=8, static_passthrough=())
        self.addCleanup(self.owner.close)
        self.runtime = SubjectAdmissionRuntime(self.owner)
        self.owner.attach_teardown(self.runtime)
        self.handler = Handler(self)
        self.proxyserver = types.SimpleNamespace(connections={})
        self.runtime._transport = (InlineLoop(), self.proxyserver)
        bound = patch.object(subject_admission, "TEARDOWN_TIMEOUT_SECONDS", 0.2)
        bound.start()
        self.addCleanup(bound.stop)
        self.sequence = 0

    def command(self, operation, target=f.A, **kwargs):
        self.sequence += 1
        return self.owner.lifecycle(operation, f.command(self.sequence, target, **kwargs))

    def register(self, target=f.A, address="192.0.2.1"):
        return self.command("register", target, sourceAddresses=[address])

    def install(self, target=f.A, epoch=1):
        item = f.snapshot(target, epoch, HOST)
        self.owner.prepare(target, item.revision, item.payload)
        self.owner.commit(target, item.revision)

    def connect(self, address="192.0.2.1", sni=HOST, *, ech=False, handshake=True):
        client = types.SimpleNamespace(peername=(address, 4000), id=object(), error=None)
        self.runtime.client_connected(client)
        self.proxyserver.connections[client.id] = self.handler
        self.addCleanup(self.runtime.client_disconnected, client)
        result = None
        if handshake:
            result = self.runtime.admit_tls(client, sni, ech_hidden=ech, destination_port=443)
        return client, result

    def live(self, client):
        return id(client) in self.runtime._clients

    def unload_counts(self, receipt):
        unload = receipt["result"]["unload"]
        return (receipt["result"]["outcome"], unload["transportObligations"], unload["completion"],
                unload["closedConnections"], unload["remainingConnections"])

    # -- AC1 / AC5: every proxied client of the identity, decrypted or opaque --

    def test_unload_closes_decrypted_and_opaque_and_spares_other_subject(self):
        self.register()
        self.register(f.B, "192.0.2.2")
        self.install()
        self.install(f.B)
        decrypted, admitted = self.connect()
        self.assertEqual(admitted.action, "decrypt")
        no_sni, result = self.connect(sni=None)
        self.assertEqual(result.reason, "no_sni")
        ech, result = self.connect(ech=True)
        self.assertEqual(result.reason, "ech")
        unhandshaken, _ = self.connect(handshake=False)
        other, other_result = self.connect("192.0.2.2")
        registry = self.owner.lookup_source("192.0.2.1").registry
        self.handler.hook = lambda _client: self.assertIsNone(self.owner.lookup_source("192.0.2.1"))
        receipt = self.command("unload")
        self.assertEqual(self.unload_counts(receipt), ("unloaded", "none", "terminal", 4, 0))
        self.assertEqual(set(map(id, self.handler.closed)),
                         {id(decrypted), id(no_sni), id(ech), id(unhandshaken)})
        self.assertEqual(registry.count, 0)  # released by the disconnect hook only
        self.assertTrue(self.live(other))
        self.assertEqual(self.runtime.acquire_request(other_result.token).action, "allow")
        self.assertEqual((self.runtime._fenced, self.runtime._fence_all), ({}, 0))

    def test_same_serial_in_other_subject_is_not_closed(self):
        self.register()
        self.register(f.B, "192.0.2.2")
        self.install()
        self.install(f.B)
        a, first = self.connect()
        b, second = self.connect("192.0.2.2")
        self.assertEqual(first.token.serial, second.token.serial)
        self.command("unload")
        self.assertEqual(self.handler.closed, [a])
        self.assertTrue(self.live(b))

    def test_source_normalization_and_unparseable_peers(self):
        self.register(address="::ffff:192.0.2.9")
        self.install()
        mapped, result = self.connect("::FFFF:c000:209")
        self.assertEqual(result.action, "decrypt")
        broken = types.SimpleNamespace(peername=None, id=object(), error=None)
        self.runtime.client_connected(broken)
        self.addCleanup(self.runtime.client_disconnected, broken)
        self.assertEqual(self.runtime.admit_tls(broken, HOST, destination_port=443).reason, "unknown_identity")
        receipt = self.command("unload")
        self.assertEqual(self.unload_counts(receipt)[3:], (1, 0))
        self.assertEqual(self.handler.closed, [mapped])

    # -- AC2: source reuse never inherits and waits for confirmed closure --

    def test_register_closes_residual_before_publication_and_never_inherits(self):
        early, _ = self.connect(handshake=False)  # no owner yet: no identity
        self.handler.hook = lambda _client: self.assertIsNone(self.owner.lookup_source("192.0.2.1"))
        self.assertEqual(self.register()["result"]["outcome"], "registered")
        self.assertEqual(self.handler.closed, [early])
        self.install()
        self.assertEqual(self.runtime.admit_tls(early, HOST, destination_port=443).reason, "unknown_identity")

    def test_unconfirmed_residual_rejects_register_until_disconnect(self):
        self.register()
        self.install()
        old, admitted = self.connect()
        registry = self.owner.lookup_source("192.0.2.1").registry
        self.handler.mode = "ignore"
        receipt = self.command("unload")
        self.assertEqual(self.unload_counts(receipt), ("subject_failed", "pending", "fenced", 0, 1))
        self.assertEqual(receipt["result"]["httpStatus"], 503)
        self.assertEqual(registry.count, 1)  # no release before the disconnect hook
        self.assertEqual(self.register(f.B)["result"]["outcome"], "subject_failed")
        self.assertIsNone(self.owner.lookup_source("192.0.2.1"))
        self.runtime.client_disconnected(old)
        self.assertEqual(registry.count, 0)
        self.assertEqual(self.register(f.B)["result"]["outcome"], "registered")
        self.install(f.B)
        fresh, result = self.connect()
        self.assertEqual(result.action, "decrypt")
        self.assertIsNot(self.owner.lookup_source("192.0.2.1").registry, registry)
        self.assertEqual(self.runtime.acquire_request(admitted.token).reason, "invalid_token")

    def test_same_key_new_generation_does_not_adopt_old_members(self):
        self.register()
        self.install()
        old, _ = self.connect()
        self.handler.mode = "ignore"
        self.command("unload")
        member = self.runtime._clients[id(old)]
        renewed = f.subject.Target(f.A.subject_key, "generation-a2")
        self.handler.mode = "disconnect"
        self.assertEqual(self.register(renewed)["result"]["outcome"], "registered")
        self.install(renewed)
        fresh, result = self.connect()
        self.assertEqual(result.action, "decrypt")
        self.assertFalse(self.live(old))  # Register confirmed the residual closed
        closes = len(self.handler.closed)
        # A late callback of the earlier teardown re-checks the exact member.
        self.runtime._teardown_in_loop(self.proxyserver, member, old)
        self.runtime.client_disconnected(old)
        self.assertEqual(len(self.handler.closed), closes)
        self.assertEqual({id(client) for client in self.handler.closed}, {id(old)})
        self.assertTrue(self.live(fresh))

    # -- AC3: suspend closes old transports; new snapshots serve new clients --

    def test_suspend_closes_old_and_reinstall_serves_only_new_connections(self):
        self.register()
        self.install()
        old, admitted = self.connect()
        handle = self.runtime.acquire_request(admitted.token).handle
        member = self.runtime._clients[id(old)]
        self.assertEqual(self.command("suspend")["result"], {"httpStatus": 200, "outcome": "suspended", "unload": None})
        self.assertEqual(self.handler.closed, [old])
        self.install(epoch=2)
        fresh, result = self.connect()
        self.assertEqual(result.action, "decrypt")
        self.runtime._teardown_in_loop(self.proxyserver, member, old)
        self.runtime.client_disconnected(old)
        self.runtime.finish_request(handle)
        self.assertEqual(self.handler.closed, [old])
        self.assertEqual(self.runtime.acquire_request(result.token).action, "allow")

    def test_unconfirmed_suspend_is_failure_and_old_member_never_revives(self):
        self.register()
        self.install()
        old, admitted = self.connect()
        self.handler.mode = "ignore"
        self.assertEqual(self.command("suspend")["result"]["outcome"], "subject_failed")
        self.install(epoch=2)
        self.assertEqual(self.runtime.acquire_request(admitted.token).reason, "connection_fenced")
        self.assertEqual(self.runtime.admit_tls(old, HOST, destination_port=443).reason, "unknown_identity")
        self.handler.mode = "disconnect"
        self.assertEqual(self.connect()[1].action, "decrypt")

    # -- AC4: every failure path tries; none reports unconfirmed completion --

    def test_no_loop_missing_handler_and_close_error_stay_remaining(self):
        for case in ("loop", "handler", "raise"):
            with self.subTest(case=case):
                self.register(C, "192.0.2.3")
                self.install(C)
                client, _ = self.connect("192.0.2.3")
                transport = self.runtime._transport
                if case == "loop":
                    self.runtime._transport = (None, None)
                elif case == "handler":
                    del self.proxyserver.connections[client.id]
                else:
                    self.handler.mode = "raise"
                receipt = self.command("unload", C)
                self.assertEqual(self.unload_counts(receipt), ("subject_failed", "pending", "fenced", 0, 1))
                self.runtime._transport, self.handler.mode = transport, "disconnect"
                self.runtime.client_disconnected(client)

    def test_receiver_failure_closes_subject_members(self):
        self.register()
        self.install()
        client, _ = self.connect()
        entry = self.owner._entries[f.A.subject_key]
        item = f.snapshot(f.A, 2, HOST)
        with patch.object(entry.receiver, "prepare", side_effect=RuntimeError("boom")):
            with self.assertRaises(f.subject.SubjectError):
                self.owner.prepare(f.A, item.revision, item.payload)
        self.assertEqual(self.handler.closed, [client])
        self.assertEqual(self.runtime._fenced, {})

    def test_terminal_owner_closes_all_and_stays_fenced(self):
        self.register()
        self.register(f.B, "192.0.2.2")
        self.install()
        a, _ = self.connect()
        b, _ = self.connect("192.0.2.2")
        item = f.snapshot(f.A, 2, HOST)
        self.owner.prepare(f.A, item.revision, item.payload)
        with patch.object(self.owner, "_publish_index", side_effect=RuntimeError("boom")):
            with self.assertRaises(f.subject.SubjectError):
                self.owner.commit(f.A, item.revision)
        self.assertEqual({id(c) for c in self.handler.closed}, {id(a), id(b)})
        late = types.SimpleNamespace(peername=("192.0.2.2", 1), id=object(), error=None)
        self.runtime.client_connected(late)
        self.assertEqual(late.error, "connection fenced")

    def test_close_on_proxy_loop_closes_inline_without_waiting(self):
        self.register()
        self.install()
        client, _ = self.connect()
        self.handler.mode = "ignore"

        async def shutdown():
            self.runtime._transport = (asyncio.get_running_loop(), self.proxyserver)
            self.owner.close()
        asyncio.run(asyncio.wait_for(shutdown(), 0.15))  # faster than the 0.2s wait bound
        self.assertEqual(self.handler.closed, [client])
        self.runtime.client_disconnected(client)

    # -- AC5: connect races Register/Unload deterministically --

    def test_connect_racing_unload_is_selected_or_sees_revoked_index(self):
        self.register()
        self.install()
        entered, release, fencing = threading.Event(), threading.Event(), threading.Event()
        lookup, fence = self.owner.lookup_source, self.runtime.fence

        def slow_lookup(address):
            dispatch = lookup(address)  # A's route, read before the Unload
            entered.set()
            release.wait(5)
            return dispatch

        def observed_fence(**selection):
            fencing.set()
            return fence(**selection)
        client = types.SimpleNamespace(peername=("192.0.2.1", 1), id=object(), error=None)
        self.proxyserver.connections[client.id] = self.handler
        with patch.object(self.owner, "lookup_source", side_effect=slow_lookup), \
                patch.object(self.runtime, "fence", side_effect=observed_fence):
            connector = threading.Thread(target=self.runtime.client_connected, args=(client,))
            connector.start()
            self.assertTrue(entered.wait(5))
            receipts = []
            unloader = threading.Thread(target=lambda: receipts.append(self.command("unload")))
            unloader.start()
            self.assertTrue(fencing.wait(5))
            release.set()  # the connect, holding the runtime lock, inserts first
            connector.join(5)
            unloader.join(5)
        self.assertEqual(self.unload_counts(receipts[0])[3:], (1, 0))
        self.assertEqual(self.handler.closed, [client])
        late = types.SimpleNamespace(peername=("192.0.2.1", 1), id=object(), error=None)
        self.runtime.client_connected(late)
        self.addCleanup(self.runtime.client_disconnected, late)
        self.assertIsNone(self.runtime._clients[id(late)].target)

    def test_connect_during_teardown_is_rejected(self):
        self.register()
        old, _ = self.connect(handshake=False)
        rejected = []

        def reconnect(_client):
            new = types.SimpleNamespace(peername=("192.0.2.1", 2), id=object(), error=None)
            self.runtime.client_connected(new)
            rejected.append(new.error)
        self.handler.hook = reconnect
        self.command("unload")
        self.register(f.B)
        self.assertEqual(rejected, ["connection fenced"])
        self.assertEqual(self.connect()[0].error, None)  # fence released with the receipt

    def connect_from(self, address="192.0.2.1"):
        """client_connected on another thread while the caller holds its locks."""
        client = types.SimpleNamespace(peername=(address, 5000), id=object(), error=None)
        self.proxyserver.connections[client.id] = self.handler
        self.addCleanup(self.runtime.client_disconnected, client)
        connector = threading.Thread(target=self.runtime.client_connected, args=(client,))
        connector.start()
        connector.join(5)
        self.assertFalse(connector.is_alive())
        return client

    def test_connect_during_initially_empty_register_is_rejected(self):
        # Codex 4573191823: no residual member, then a connect lands while the
        # receiver is constructed, before the new owner is published.
        raced = []
        receiver = f.subject.InstallationReceiver

        def constructing(*args, **kwargs):
            self.assertTrue(self.owner._operation_lock.locked())
            raced.append(self.connect_from())
            return receiver(*args, **kwargs)
        with patch.object(f.subject, "InstallationReceiver", side_effect=constructing):
            self.assertEqual(self.register()["result"]["outcome"], "registered")
        self.assertEqual(raced[0].error, "connection fenced")
        self.assertFalse(self.live(raced[0]))
        self.assertEqual(self.handler.closed, [])
        self.assertEqual(self.runtime._fenced, {})  # released after publication
        self.install()
        fresh, result = self.connect()
        self.assertEqual((fresh.error, result.action), (None, "decrypt"))
        self.assertEqual(self.runtime.admit_tls(raced[0], None, destination_port=443).reason,
                         "unknown_identity")

    def test_empty_register_construction_failure_releases_its_fence(self):
        raced = []

        def failing(*_args, **_kwargs):
            raced.append(self.connect_from())
            raise RuntimeError("boom")
        with patch.object(f.subject, "InstallationReceiver", side_effect=failing):
            self.assertEqual(self.register()["result"]["outcome"], "subject_failed")
        self.assertEqual(raced[0].error, "connection fenced")
        self.assertEqual((self.runtime._fenced, self.owner._pending), ({}, None))
        self.assertIsNone(self.owner.lookup_source("192.0.2.1"))
        self.assertEqual(self.register()["result"]["outcome"], "registered")
        self.assertEqual(self.runtime._fenced, {})

    def test_conflict_and_capacity_rejections_take_no_fence(self):
        self.register()
        fence = self.runtime.fence
        with patch.object(self.runtime, "fence", side_effect=fence) as observed:
            self.assertEqual(self.register(f.B)["result"]["outcome"], "subject_conflict")
            self.register(f.B, "192.0.2.2")
            self.register(C, "192.0.2.3")
            observed.reset_mock()
            full = f.subject.Target("s-d", "generation-d")
            self.assertEqual(self.register(full, "192.0.2.4")["result"]["outcome"],
                             "subject_capacity_exhausted")
        observed.assert_not_called()
        self.assertEqual(self.runtime._fenced, {})

    def reconnect_during_teardown(self):
        """Close hook: a no-SNI reconnect from the source while teardown waits."""
        attempts = []

        def reconnect(_client):
            new = self.connect_from()
            attempts.append((new.error, self.live(new),
                             self.runtime.admit_tls(new, None, destination_port=443).reason))
        self.handler.hook = reconnect
        return attempts

    def test_reconnect_during_suspend_teardown_is_rejected(self):
        # Codex 4573191825: Suspend must fence the source, not only the target.
        self.register()
        self.install()
        old, admitted = self.connect()
        attempts = self.reconnect_during_teardown()
        self.assertEqual(self.command("suspend")["result"]["outcome"], "suspended")
        self.assertEqual(attempts, [("connection fenced", False, "unknown_identity")])
        self.assertEqual((self.runtime._fenced, self.handler.closed), ({}, [old]))
        self.handler.hook = None
        self.install(epoch=2)  # a later commit restores new connections
        fresh, result = self.connect()
        self.assertEqual((fresh.error, result.action), (None, "decrypt"))
        self.assertEqual(self.runtime.acquire_request(admitted.token).reason, "invalid_token")
        self.assertEqual(self.runtime.admit_tls(old, HOST, destination_port=443).reason,
                         "unknown_identity")

    def test_reconnect_during_receiver_failure_teardown_is_rejected(self):
        self.register()
        self.install()
        old, _ = self.connect()
        attempts = self.reconnect_during_teardown()
        entry = self.owner._entries[f.A.subject_key]
        item = f.snapshot(f.A, 2, HOST)
        with patch.object(entry.receiver, "prepare", side_effect=RuntimeError("boom")):
            with self.assertRaises(f.subject.SubjectError):
                self.owner.prepare(f.A, item.revision, item.payload)
        self.assertEqual(attempts, [("connection fenced", False, "unknown_identity")])
        self.assertEqual((self.runtime._fenced, self.handler.closed), ({}, [old]))
        self.handler.hook = None
        later, _ = self.connect(handshake=False)
        self.assertEqual(later.error, None)  # the fence is not permanent
        self.assertFalse(self.live(old))

    # -- AC6: frozen receipts across exact replay, status and concurrency --

    def test_pending_teardown_freezes_receipt_without_holding_owner_lock(self):
        self.register()
        self.register(f.B, "192.0.2.2")
        self.install()
        client, _ = self.connect()
        closing, finish = threading.Event(), threading.Event()
        self.handler.mode = "ignore"

        def blocked(_client):
            closing.set()
            finish.wait(5)
            self.runtime.client_disconnected(_client)
        self.handler.hook = blocked
        raw = f.command(3, f.A)
        results = []
        first = threading.Thread(target=lambda: results.append(self.owner.lifecycle("unload", raw)))
        first.start()
        self.assertTrue(closing.wait(5))
        # Not under the operation lock: other subjects and status still answer.
        self.assertIsNone(self.owner.readback(f.B))
        self.assertEqual(self.owner.lifecycle_status(f.CONTROL)["lastSequence"], 2)
        replay = threading.Thread(target=lambda: results.append(self.owner.lifecycle("unload", raw)))
        replay.start()
        finish.set()
        first.join(5)
        replay.join(5)
        self.assertEqual(results[0], results[1])
        status = self.owner.lifecycle_status(f.CONTROL)
        self.assertEqual({"lifecycleSequence": status.pop("lastSequence"), **status}, results[0])
        self.assertEqual(results[0]["result"]["unload"]["closedConnections"], 1)
        self.sequence = 3
        self.assertEqual(self.command("unload")["result"]["outcome"], "already_absent")

    # -- AC8 / AC9 --------------------------------------------------------

    def test_installation_only_receipt_and_owner_reject_teardown(self):
        owner = f.make_owner()
        self.addCleanup(owner.close)
        owner.lifecycle("register", f.command(1, sourceAddresses=["192.0.2.1"]))
        self.assertEqual(owner.lifecycle("unload", f.command(2))["result"]["unload"],
                         {"routingRevoked": True, "transportObligations": "none", "completion": "terminal"})
        with self.assertRaises(ValueError):
            owner.attach_teardown(SubjectAdmissionRuntime(owner))
        with self.assertRaises(ValueError):
            self.owner.attach_teardown(self.runtime)

    def test_churn_keeps_no_member_or_fence_history(self):
        for index in range(50):
            target = f.subject.Target(f"s-{index}", f"g-{index}")
            self.register(target)
            self.install(target)
            self.connect()
            self.connect(sni=None)
            self.assertEqual(self.unload_counts(self.command("unload", target))[3:], (2, 0))
        self.assertEqual((self.runtime._clients, self.runtime._fenced, self.runtime._fence_all,
                          self.runtime._connections, self.runtime._tokens), ({}, {}, 0, {}, {}))
        self.assertIsNone(self.owner._pending)


if __name__ == "__main__":
    unittest.main()
