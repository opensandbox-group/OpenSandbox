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


if __name__ == "__main__":
    unittest.main()
