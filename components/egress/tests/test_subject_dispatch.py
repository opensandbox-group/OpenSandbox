# Copyright 2026 The OpenSandbox Authors
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# http://www.apache.org/licenses/LICENSE-2.0

"""Installation-only dispatch inspection; these tests never enable TLS hooks."""

import threading
import unittest
from unittest import mock

import test_subject_revision as f

s = f.subject


class DispatchTest(unittest.TestCase):
    def setUp(self):
        self.owner = f.make_owner(max_subjects=3, retained_payload_budget=12288)
        self.sequence = 0
        self.addCleanup(self.owner.close)

    def command(self, op, target=f.A, **kwargs):
        self.sequence += 1
        return self.owner.lifecycle(op, f.command(self.sequence, target, **kwargs))["result"]

    def register(self, addresses, target=f.A):
        return self.command("register", target, sourceAddresses=addresses)

    def lookup(self, address="192.0.2.1"):
        self.assertTrue(callable(getattr(self.owner, "lookup_source", None)), "missing dispatch inspection")
        return self.owner.lookup_source(address)

    def install(self, epoch=1):
        item = f.snapshot(epoch=epoch)
        self.owner.prepare(f.A, item.revision, item.payload)
        self.owner.commit(f.A, item.revision)
        return item

    def test_canonical_addresses_and_immutable_index(self):
        addresses = ["192.0.2.1", "2001:0DB8:0000::1", "::ffff:192.0.2.2"]
        self.assertEqual(self.register(addresses)["outcome"], "registered")
        for address in ["192.0.2.1", "2001:db8::1", "::ffff:c000:202"]:
            view = self.lookup(address)
            self.assertEqual(view.target, f.A)
            self.assertIsNone(view.snapshot)
        old = self.owner._source_index
        with self.assertRaises(TypeError):
            old["192.0.2.9"] = None
        item = self.install()
        self.assertEqual(self.lookup().snapshot, item)
        self.assertIsNone(old["192.0.2.1"].snapshot)
        self.assertEqual(self.command("unload")["outcome"], "unloaded")
        self.assertIsNone(self.lookup())
        self.register(["192.0.2.1"], f.B)
        self.assertEqual(self.lookup().target, f.B)

    def test_bad_addresses_do_not_consume_sequence(self):
        for address in ["fe80::1%eth0", "fe80::1%1", "not-ip", "1.2.3.999", "192.168.001.1", " 192.0.2.1", "[::1]"]:
            with self.subTest(address=address):
                with self.assertRaises(s.SubjectError) as caught:
                    self.owner.lifecycle("register", f.command(1, sourceAddresses=[address]))
                self.assertEqual(caught.exception.outcome, "malformed_request")
                self.assertEqual(self.owner.lifecycle_status(f.CONTROL)["lastSequence"], 0)
        self.assertEqual(self.register(["::1"])["httpStatus"], 200)

    def test_conflict_preserves_old_routes_atomically(self):
        self.assertEqual(self.register(["192.0.2.1"])["httpStatus"], 200)
        self.assertEqual(self.register(["192.0.2.2", "192.0.2.1"], f.B)["outcome"], "subject_conflict")
        self.assertEqual(self.lookup().target, f.A)
        self.assertIsNone(self.lookup("192.0.2.2"))
        self.command("unload")
        self.assertEqual(self.register(["192.0.2.1"], f.B)["outcome"], "registered")

    def test_unload_revokes_before_close_and_quarantines_capacity(self):
        self.register(["192.0.2.1"])
        receiver = self.owner._entries[f.A.subject_key].receiver
        def fail_close():
            self.assertIsNone(self.lookup(), "route must disappear before receiver close/receipt")
            self.assertNotIn(f.A.subject_key, self.owner._entries)
            raise RuntimeError("fixture close failure")
        with mock.patch.object(receiver, "close", side_effect=fail_close):
            result = self.command("unload")
        self.assertEqual(result["outcome"], "subject_failed")
        self.assertEqual(result["unload"]["completion"], "fenced")
        self.assertEqual(self.register(["192.0.2.1"])["outcome"], "registered")
        self.assertEqual(self.register(["192.0.2.2"], f.B)["outcome"], "registered")
        target = s.Target("s-c", "generation-c")
        self.assertEqual(self.register(["192.0.2.3"], target)["outcome"], "subject_capacity_exhausted")
        self.assertEqual(len(self.owner._quarantined), 1)

    def test_suspend_fences_prepared_commit_and_exact_active_retry(self):
        self.register(["192.0.2.1"])
        old = self.install()
        pending = f.snapshot(epoch=2)
        self.owner.prepare(f.A, pending.revision, pending.payload)
        self.assertEqual(self.command("suspend")["outcome"], "suspended")
        self.assertIsNone(self.lookup().snapshot)
        self.owner.commit(f.A, pending.revision)
        self.assertIsNone(self.lookup().snapshot, "pre-suspend prepare must not restore authority")
        self.assertIsNone(self.owner.acquire(f.A))
        self.assertEqual(self.owner.readback(f.A), pending.revision)
        self.assertEqual(self.command("suspend")["outcome"], "suspended")
        self.owner.commit(f.A, pending.revision)
        self.assertIsNone(self.lookup().snapshot)
        self.install(epoch=3)
        self.assertIsNotNone(self.lookup().snapshot)
        self.assertNotEqual(self.lookup().snapshot.revision, old.revision)

    def check_active_retry_preserves_pending_dispatch(self, suspended):
        self.register(["192.0.2.1"])
        active = self.install()
        if suspended:
            self.assertEqual(self.command("suspend")["outcome"], "suspended")
            self.assertIsNone(self.lookup().snapshot)
        pending = f.snapshot(epoch=2)
        self.owner.prepare(f.A, pending.revision, pending.payload)
        self.owner.prepare(f.A, active.revision, active.payload)
        self.owner.commit(f.A, pending.revision)
        self.assertEqual(self.owner.readback(f.A), pending.revision)
        self.assertEqual(self.lookup().snapshot, pending)
        self.assertEqual(self.owner.acquire(f.A), pending)

    def test_active_prepare_retry_preserves_pending_dispatch(self):
        self.check_active_retry_preserves_pending_dispatch(suspended=False)

    def test_suspended_active_prepare_retry_preserves_pending_dispatch(self):
        self.check_active_retry_preserves_pending_dispatch(suspended=True)

    def test_suspend_failure_table_digest_and_exact_replay(self):
        self.assertEqual(self.command("suspend")["outcome"], "subject_unknown")
        self.register(["192.0.2.1"])
        other = s.Target(f.A.subject_key, "other-generation")
        self.assertEqual(self.command("suspend", other)["outcome"], "subject_generation_mismatch")
        raw = f.command(4)
        first = self.owner.lifecycle("suspend", raw)
        self.assertEqual(first, self.owner.lifecycle("suspend", raw))
        self.assertEqual(first["commandDigest"], s.command_digest("suspend", raw))
        self.assertNotEqual(s.command_digest("suspend", raw), s.command_digest("unload", raw))

    def test_lookup_does_not_wait_for_operation_lock_or_slow_prepare(self):
        self.register(["192.0.2.1"])
        self.install()
        entered, release, read_done = threading.Event(), threading.Event(), threading.Event()
        receiver = self.owner._entries[f.A.subject_key].receiver
        real = receiver.prepare
        def slow(*args):
            entered.set()
            self.assertTrue(release.wait(3))
            return real(*args)
        item = f.snapshot(epoch=2)
        with mock.patch.object(receiver, "prepare", side_effect=slow):
            thread = threading.Thread(target=self.owner.prepare, args=(f.A, item.revision, item.payload))
            thread.start()
            self.assertTrue(entered.wait(3))
            def read():
                self.assertEqual(self.lookup().target, f.A)
                read_done.set()
            reader = threading.Thread(target=read)
            reader.start()
            try:
                self.assertTrue(read_done.wait(1), "lookup blocked on the operation lock")
            finally:
                release.set()
                thread.join(3)
                reader.join(3)

    def test_owner_replacement_and_close_clear_routes(self):
        self.register(["192.0.2.1"])
        other = s.SubjectRevisionOwner("replacement", max_subjects=1,
                                      max_snapshot_bytes=2048, retained_payload_budget=4096)
        self.addCleanup(other.close)
        self.assertIsNone(other.lookup_source("192.0.2.1"))
        self.owner.close()
        self.assertIsNone(self.lookup())


if __name__ == "__main__":
    unittest.main()
