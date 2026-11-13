# Copyright 2026 The OpenSandbox Authors
# SPDX-License-Identifier: Apache-2.0

"""Fast Sandbox dispatch-only TLS admission and shared atomic accounting."""

import threading
import types
import unittest
from concurrent.futures import ThreadPoolExecutor
from unittest.mock import patch

import test_subject_revision as f
import test_tls_registry as tls


class SubjectAdmissionTest(unittest.TestCase):
    def setUp(self):
        self.owner = f.make_owner(tls_capacity=2, process_tls_capacity=3, static_passthrough=())
        self.addCleanup(self.owner.close)
        self.sequence = 0
        self.command("register", sourceAddresses=["192.0.2.1"])
        self.command("register", f.B, sourceAddresses=["192.0.2.2"])

    def command(self, op, target=f.A, **kwargs):
        self.sequence += 1
        return self.owner.lifecycle(op, f.command(self.sequence, target, **kwargs))

    def install(self, target=f.A, epoch=1, host="api.example.com"):
        item = f.snapshot(target, epoch, host)
        self.owner.prepare(target, item.revision, item.payload)
        self.owner.commit(target, item.revision)
        return item

    def admit(self, address="192.0.2.1", sni="api.example.com", ech=False, port=443):
        return self.owner.admit_tls(address, sni, ech_hidden=ech, destination_port=port)

    def assert_reason(self, result, action, reason):
        self.assertEqual((result.action, result.reason), (action, reason))

    def test_admission_configuration_requires_complete_triple(self):
        fields = {"tls_capacity": 2, "process_tls_capacity": 3, "static_passthrough": ()}
        for mask in range(1, 7):
            supplied = {name: value for i, (name, value) in enumerate(fields.items()) if mask & (1 << i)}
            with self.subTest(supplied=supplied), self.assertRaises(ValueError):
                f.make_owner(**supplied)

    def test_admission_configuration_all_or_none_initializes_fixed_state(self):
        for fields in ({}, dict(tls_capacity=2, process_tls_capacity=3, static_passthrough=())):
            with self.subTest(fields=fields):
                owner = f.make_owner(**fields)
                self.addCleanup(owner.close)
                enabled = bool(fields)
                self.assertEqual(owner._tls_capacity, 2 if enabled else None)
                self.assertEqual(owner._static_passthrough, ())
                self.assertEqual(owner._accounting is not None, enabled)
                self.assertEqual(owner.lifecycle("register", f.command(1))["result"]["outcome"], "registered")
                self.assertEqual(owner._entries[f.A.subject_key].registry is not None, enabled)

    def test_static_selectors_identity_skips_only_startup_validation(self):
        from subject_admission import SubjectRegistry

        registry = SubjectRegistry(capacity=2, accounting=tls.registry_module.ConnectionAccounting(3),
                                   static_passthrough=(tls.selectors.parse_canonical("static.example.com"),))
        selectors = registry.static_selectors
        equivalent = tuple(list(selectors))
        self.assertEqual(selectors, equivalent)
        self.assertIsNot(selectors, equivalent)
        self.assertIs(registry.static_selectors, selectors)
        with patch.object(tls.decision, "validate_selectors", wraps=tls.decision.validate_selectors) as validate:
            for value in (selectors, equivalent):
                result = registry.admit(identity=(f.CONTROL, f.A.subject_generation),
                                        sni="static.example.com", ech_hidden=False,
                                        static_passthrough=value, destination_port=443)
                self.assert_reason(result, "passthrough", "static_ignore")
                if value is selectors:
                    validate.assert_not_called()
                else:
                    validate.assert_called_once_with(equivalent)

    def test_decision_order_identity_early_bypass_snapshot_binding_and_registry(self):
        self.assert_reason(self.admit("192.0.2.9", ech=True), "deny", "unknown_identity")
        self.assert_reason(self.admit("192.0.2.9", sni=None), "deny", "unknown_identity")
        self.assert_reason(self.admit(ech=True, port=8443), "passthrough", "ech")
        self.assert_reason(self.admit(sni=None, port=8443), "passthrough", "no_sni")
        self.assert_reason(self.admit(sni="bad name"), "deny", "invalid_sni")
        self.assert_reason(self.admit(), "deny", "snapshot_missing")
        self.install()
        self.install(f.B, host=None)
        self.assert_reason(self.admit(), "decrypt", "binding_host")
        self.assert_reason(self.admit("192.0.2.2"), "passthrough", "no_binding_host")
        self.assert_reason(self.admit(port=8443), "deny", "non_canonical_port")
        registry = self.owner.lookup_source("192.0.2.1").registry
        self.assert_reason(registry.admit(identity=(f.CONTROL, "other-generation"),
                           sni="api.example.com", ech_hidden=False, static_passthrough=(),
                           destination_port=443), "deny", "generation_mismatch")

    def test_static_before_snapshot_but_after_noncanonical_port(self):
        owner = f.make_owner(tls_capacity=2, process_tls_capacity=3,
                             static_passthrough=(tls.selectors.parse_canonical("static.example.com"),))
        self.addCleanup(owner.close)
        owner.lifecycle("register", f.command(1, sourceAddresses=["192.0.2.3"]))
        for sni, ech, port, action, reason in [
            ("static.example.com", False, 443, "passthrough", "static_ignore"),
            ("static.example.com", False, 8443, "deny", "non_canonical_port"),
            ("static.example.com", True, 8443, "passthrough", "ech"),
            (None, False, 8443, "passthrough", "no_sni"),
        ]:
            self.assert_reason(owner.admit_tls("192.0.2.3", sni, ech_hidden=ech,
                                             destination_port=port), action, reason)

    def test_two_budgets_release_exactly_once_and_opaque_never_consumes(self):
        self.install()
        self.install(f.B)
        a = [self.admit() for _ in range(2)]
        self.assert_reason(self.admit(), "deny", "registry_exhausted")
        b = self.admit("192.0.2.2")
        self.assert_reason(b, "decrypt", "binding_host")
        self.assert_reason(self.admit("192.0.2.2"), "deny", "registry_exhausted")
        for address in ("192.0.2.1", "192.0.2.2"):
            self.assert_reason(self.admit(address, "unbound.example.com"), "passthrough", "no_binding_host")
        registry = self.owner.lookup_source("192.0.2.1").registry
        self.assertTrue(registry.release(a[0].token))
        self.assertFalse(registry.release(a[0].token))
        self.assert_reason(self.admit("192.0.2.2"), "decrypt", "binding_host")
        self.assertEqual(registry.count, 1)  # existing token was not evicted

    def test_concurrent_handshakes_never_oversubscribe_process_or_subject(self):
        self.install()
        self.install(f.B)
        barrier = threading.Barrier(16)
        def handshake(i):
            barrier.wait(5)
            address = "192.0.2.1" if i % 2 else "192.0.2.2"
            return address, self.admit(address)
        with ThreadPoolExecutor(max_workers=16) as pool:
            results = list(pool.map(handshake, range(16)))
        admitted = [a for a, result in results if result.action == "decrypt"]
        self.assertEqual(len(admitted), 3)
        self.assertLessEqual(admitted.count("192.0.2.1"), 2)
        self.assertLessEqual(admitted.count("192.0.2.2"), 2)

    def test_dispatch_only_suspend_old_prepare_commit_and_unload_stale_reader(self):
        self.install()
        old = self.owner.lookup_source("192.0.2.1")
        pending = f.snapshot(epoch=2, host="api.example.com")
        self.owner.prepare(f.A, pending.revision, pending.payload)
        self.command("suspend")
        receiver = self.owner._entries[f.A.subject_key].receiver
        with patch.object(receiver, "acquire", side_effect=AssertionError("receiver admission forbidden")), \
                patch.object(receiver, "readback", side_effect=AssertionError("receiver admission forbidden")):
            self.assert_reason(self.admit(), "deny", "snapshot_missing")
            self.assert_reason(self.admit(ech=True), "passthrough", "ech")
        self.owner.commit(f.A, pending.revision)
        self.assert_reason(self.admit(), "deny", "snapshot_missing")
        self.install(epoch=3)
        self.assert_reason(self.admit(), "decrypt", "binding_host")
        self.command("unload")
        self.assert_reason(old.registry.admit(identity=(f.CONTROL, f.A.subject_generation),
                           sni="api.example.com", ech_hidden=False,
                           static_passthrough=(), destination_port=443), "deny", "snapshot_missing")
        self.command("register", f.B, sourceAddresses=["192.0.2.1"])  # conflict, old registry stays fenced
        self.assert_reason(self.admit(), "deny", "unknown_identity")

    def test_handshake_does_not_wait_for_slow_install(self):
        self.install()
        entered, resume, done = threading.Event(), threading.Event(), threading.Event()
        receiver = self.owner._entries[f.A.subject_key].receiver
        prepare = receiver.prepare
        def slow(*args):
            entered.set()
            self.assertTrue(resume.wait(5))
            return prepare(*args)
        item = f.snapshot(epoch=2, host="api.example.com")
        with patch.object(receiver, "prepare", side_effect=slow):
            worker = threading.Thread(target=self.owner.prepare, args=(f.A, item.revision, item.payload))
            worker.start()
            self.assertTrue(entered.wait(5))
            def read():
                self.assert_reason(self.admit(), "decrypt", "binding_host")
                done.set()
            reader = threading.Thread(target=read)
            reader.start()
            try:
                self.assertTrue(done.wait(1))
            finally:
                resume.set()
                worker.join(5)
                reader.join(5)

    def test_runtime_disconnect_releases_original_registry_after_ip_generation_reuse(self):
        from subject_admission import SubjectAdmissionRuntime

        self.install()
        runtime = SubjectAdmissionRuntime(self.owner)
        client = types.SimpleNamespace(peername=("192.0.2.1", 1234))
        first = runtime.admit_tls(client, "api.example.com", destination_port=443)
        self.assert_reason(first, "decrypt", "binding_host")
        old = self.owner.lookup_source("192.0.2.1").registry
        self.command("unload")
        new_target = f.subject.Target(f.A.subject_key, "new-generation")
        self.command("register", new_target, sourceAddresses=["192.0.2.1"])
        self.install(new_target)
        new = self.owner.lookup_source("192.0.2.1").registry
        self.assertIsNot(new, old)
        self.assertEqual(old.count, 1)
        self.assertEqual(new.budget_observations()[0], ("process", 1, 3))
        runtime.client_disconnected(client)
        runtime.client_disconnected(client)
        self.assertEqual(old.count, 0)
        self.assertEqual(new.budget_observations()[0], ("process", 0, 3))
        self.assert_reason(self.admit(), "decrypt", "binding_host")

    def runtime_connection(self):
        from subject_admission import SubjectAdmissionRuntime
        self.install()
        runtime = SubjectAdmissionRuntime(self.owner)
        client = types.SimpleNamespace(peername=("192.0.2.1", 1234))
        token = runtime.admit_tls(client, "api.example.com", destination_port=443).token
        self.addCleanup(runtime.client_disconnected, client)
        return runtime, client, token, self.owner.lookup_source("192.0.2.1").registry

    def test_request_routes_to_handshake_registry_without_source_lookup(self):
        runtime, client, token, registry = self.runtime_connection()
        client.peername = ("192.0.2.2", 1234)
        with patch.object(self.owner, "lookup_source", side_effect=AssertionError("no IP lookup")):
            self.assertIs(runtime.token_for(client), token)
            self.assertIsNone(runtime.token_for(types.SimpleNamespace()))
            self.assertIsNone(runtime.h2_for(client))
            result = runtime.acquire_request(token)
            self.assertEqual(result.action, "allow")
            self.assertEqual(result.snapshot.revision, registry._dispatch_revision)
            self.assertEqual(registry.pending_requests()[0].revision, result.snapshot.revision)
            runtime.finish_request(result.handle)
            runtime.finish_request(result.handle)
            self.assertEqual(registry.pending_requests(), ())
        self.assertEqual(runtime.acquire_request(None).reason, "invalid_token")

    def test_requests_deny_fenced_deactivated_and_missing_snapshot(self):
        for state in ("fenced", "deactivated", "missing"):
            with self.subTest(state=state):
                runtime, client, token, registry = self.runtime_connection()
                if state == "fenced":
                    registry.publish_dispatch(f.snapshot(epoch=2, host=None))
                elif state == "deactivated":
                    registry.deactivate()
                else:
                    registry.publish_dispatch(None)
                self.assertEqual(runtime.acquire_request(token).action, "deny")
                runtime.client_disconnected(client)
                # Restore a fresh entry lifetime for the next state.
                self.command("unload")
                self.command("register", sourceAddresses=["192.0.2.1"])

    def test_rotation_pins_old_handle_new_request_uses_new_snapshot(self):
        runtime, client, token, registry = self.runtime_connection()
        first = runtime.acquire_request(token)
        replacement = tls.TLSRegistryTest().snapshot(
            epoch=2, identity=(f.CONTROL, f.A.subject_generation), secret="rotated")
        self.owner.prepare(f.A, replacement.revision, replacement.payload)
        self.owner.commit(f.A, replacement.revision)
        second = runtime.acquire_request(token)
        self.assertEqual(second.action, "allow")
        self.assertEqual(second.snapshot, replacement)
        self.assertEqual(first.handle.snapshot, first.snapshot)
        self.assertNotEqual(first.snapshot.payload, second.snapshot.payload)
        runtime.finish_request(first.handle)
        runtime.client_disconnected(client)
        self.assertEqual(registry.pending_requests(), ())
        runtime.finish_request(second.handle)
        self.assertIsNone(runtime.token_for(client))

    def test_suspend_reinstall_permanently_fences_old_membership_and_keeps_slot(self):
        runtime, client, token, registry = self.runtime_connection()
        first = runtime.acquire_request(token)
        self.command("suspend")
        self.assertEqual(runtime.acquire_request(token).action, "deny")
        self.install(epoch=2)
        self.assertEqual(runtime.acquire_request(token).reason, "connection_fenced")
        self.assertEqual(first.handle.snapshot, first.snapshot)
        self.assertEqual(registry.count, 1)
        self.assertEqual(registry.budget_observations()[0], ("process", 1, 3))
        fresh = self.admit()
        self.assertEqual(registry.acquire_request(fresh.token).action, "allow")
        registry.release(fresh.token)
        runtime.finish_request(first.handle)
        runtime.client_disconnected(client)
        self.assertEqual(registry.budget_observations()[0], ("process", 0, 3))

    def test_unload_drops_dispatch_snapshot_but_preserves_pinned_request(self):
        runtime, client, token, registry = self.runtime_connection()
        admitted = runtime.acquire_request(token)
        self.command("unload")
        self.assertIsNone(registry._dispatch_snapshot)
        self.assertIsNone(registry._dispatch_revision)
        self.assertIsNotNone(admitted.handle.snapshot)
        self.assertEqual(admitted.handle.snapshot, admitted.snapshot)
        self.assertEqual(runtime.acquire_request(token).reason, "registry_closed")
        self.assertEqual(registry.deactivate(), (token,))
        self.assertEqual(registry.count, 1)
        runtime.finish_request(admitted.handle)
        runtime.client_disconnected(client)
        self.assertEqual(registry.count, 0)

    def test_repeated_unload_register_retains_no_retired_dispatch_snapshots(self):
        from subject_admission import SubjectAdmissionRuntime
        owner = f.make_owner(tls_capacity=1, process_tls_capacity=16, static_passthrough=())
        self.addCleanup(owner.close)
        runtime = SubjectAdmissionRuntime(owner)
        retired = []
        for i in range(16):
            owner.lifecycle("register", f.command(2 * i + 1, sourceAddresses=["192.0.2.3"]))
            snapshot = f.snapshot(epoch=i + 1, host="api.example.com")
            owner.prepare(f.A, snapshot.revision, snapshot.payload)
            owner.commit(f.A, snapshot.revision)
            client = types.SimpleNamespace(peername=("192.0.2.3", 1234))
            self.addCleanup(runtime.client_disconnected, client)
            self.assertEqual(runtime.admit_tls(client, "api.example.com", destination_port=443).action,
                             "decrypt")
            retired.append(owner.lookup_source("192.0.2.3").registry)
            owner.lifecycle("unload", f.command(2 * i + 2))
        self.assertEqual(len(owner._entries), 0)
        self.assertEqual(sum(registry.count for registry in retired), 16)
        self.assertEqual(sum(registry._dispatch_snapshot is not None for registry in retired), 0)

    def test_unchanged_or_closed_dispatch_does_not_compile(self):
        import subject_admission
        self.install()
        self.install(f.B)
        registry = self.owner.lookup_source("192.0.2.1").registry
        with patch.object(subject_admission, "compile_view", wraps=subject_admission.compile_view) as compile:
            self.install(f.B, epoch=2)
            self.assertEqual(compile.call_count, 1)  # A is unchanged when B republishes the index.
            compile.reset_mock()
            snapshot = registry._dispatch_snapshot
            registry.publish_dispatch(snapshot)
            registry.deactivate()
            registry.publish_dispatch(snapshot)
            compile.assert_not_called()

    def test_publish_rechecks_after_compile_when_duplicate_or_deactivated(self):
        import subject_admission
        for state in ("duplicate", "deactivated"):
            with self.subTest(state=state):
                registry = subject_admission.SubjectRegistry(
                    capacity=2, accounting=tls.registry_module.ConnectionAccounting(3), static_passthrough=())
                registry.publish_dispatch(f.snapshot(host="api.example.com"))
                replacement = f.snapshot(epoch=2, host="api.example.com")
                entered, resume = threading.Event(), threading.Event()
                errors = []
                compile = subject_admission.compile_view
                def slow(snapshot):
                    if threading.current_thread() is writer:
                        entered.set()
                        if not resume.wait(5):
                            raise AssertionError("compile resume timed out")
                    return compile(snapshot)
                def publish():
                    try:
                        registry.publish_dispatch(replacement)
                    except Exception as error:
                        errors.append(error)
                writer = threading.Thread(target=publish)
                with patch.object(subject_admission, "compile_view", side_effect=slow), \
                        patch.object(registry, "_publish_activation", wraps=registry._publish_activation) as activate:
                    writer.start()
                    try:
                        self.assertTrue(entered.wait(5))
                        if state == "duplicate":
                            registry.publish_dispatch(replacement)
                        else:
                            registry.deactivate()
                    finally:
                        resume.set()
                        writer.join(5)
                    self.assertFalse(writer.is_alive())
                    self.assertEqual(errors, [])
                    self.assertEqual(activate.call_count, 1 if state == "duplicate" else 0)
                self.assertEqual(registry._dispatch_snapshot, replacement if state == "duplicate" else None)

    def test_request_snapshot_and_dispatch_publication_share_registry_lock(self):
        runtime, client, token, registry = self.runtime_connection()
        lock = tls.ObservedLock()
        registry._lock = lock
        first = runtime.acquire_request(token)
        runtime.finish_request(first.handle)
        replacement = f.snapshot(epoch=2, host="api.example.com")
        publish = registry._publish_activation
        def pause_publication(activation):
            publish(activation)
            lock.pause_next_exit = True
        with patch.object(registry, "_publish_activation", side_effect=pause_publication):
            writer = threading.Thread(target=registry.publish_dispatch, args=(replacement,))
            writer.start()
            self.assertTrue(lock.before_unlock.wait(5))
        results, done = [], threading.Event()
        def acquire():
            results.append(runtime.acquire_request(token))
            done.set()
        lock.attempted.clear()
        reader = threading.Thread(target=acquire)
        reader.start()
        try:
            self.assertTrue(lock.attempted.wait(5))
            self.assertFalse(done.is_set())
        finally:
            lock.resume.set()
            writer.join(5)
            reader.join(5)
        self.assertTrue(done.is_set())
        self.assertEqual(results[0].snapshot, replacement)
        self.assertEqual(results[0].snapshot.revision, registry._dispatch_revision)
        runtime.finish_request(results[0].handle)

    def test_old_token_cannot_acquire_reused_ip_subject_snapshot(self):
        runtime, client, token, old = self.runtime_connection()
        self.command("unload")
        successor = f.subject.Target(f.A.subject_key, "successor")
        self.command("register", successor, sourceAddresses=["192.0.2.1"])
        self.install(successor)
        with patch.object(self.owner, "lookup_source", side_effect=AssertionError("no IP lookup")):
            self.assertEqual(runtime.acquire_request(token).action, "deny")
        self.assertEqual(old.count, 1)


if __name__ == "__main__":
    unittest.main()
