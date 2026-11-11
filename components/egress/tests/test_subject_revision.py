# Copyright 2026 The OpenSandbox Authors
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Bounded subject ownership, replay fencing, failure isolation and lock races."""

import gc
import hashlib
import json
import sys
import threading
import unittest
import weakref
from dataclasses import replace
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "mitmscripts"))

import test_tls_registry as fixtures

subject = fixtures.load("subject_revision")
receiver = fixtures.receiver
CONTROL = "control-a"
A = subject.Target("s-a", "generation-a")
B = subject.Target("s-b", "generation-b")


def wire(target):
    return {"subjectKey": target.subject_key, "subjectGeneration": target.subject_generation}


def command(sequence, target=A, *, control=CONTROL, **kwargs):
    value = {"controlGeneration": control, "lifecycleSequence": sequence,
             "target": wire(target) if type(target) is subject.Target else target}
    value.update(kwargs)
    return json.dumps(value, ensure_ascii=True, separators=(",", ":")).encode()


def make_owner(**kwargs):
    return subject.SubjectRevisionOwner(CONTROL, **{
        "max_subjects": 2, "max_snapshot_bytes": 2048, "retained_payload_budget": 8192, **kwargs,
    })


def snapshot(target=A, epoch=1, host=None):
    return fixtures.TLSRegistryTest().snapshot(epoch=epoch, host=host,
                                              identity=(CONTROL, target.subject_generation))


class SubjectOwnerTest(unittest.TestCase):
    def setUp(self):
        self.owner = make_owner()

    def tearDown(self):
        self.owner.close()

    def assert_error(self, code, operation, *args):
        with self.assertRaises(subject.SubjectError) as caught:
            operation(*args)
        self.assertEqual(caught.exception.outcome, code)
        self.assertIsNone(caught.exception.__context__)
        self.assertNotIn("secret", repr(caught.exception))
        return caught.exception

    def register(self, sequence, target=A):
        return self.owner.lifecycle("register", command(sequence, target))

    def install(self, target=A, epoch=1, host=None):
        item = snapshot(target, epoch, host)
        self.assertEqual(self.owner.prepare(target, item.revision, item.payload), item.revision)
        self.assertEqual(self.owner.commit(target, item.revision), item.revision)
        return item

    def test_unknown_uninstalled_empty_and_independent_same_epoch(self):
        self.assert_error("subject_unknown", self.owner.readback, A)
        self.register(1)
        self.assertIsNone(self.owner.readback(A))
        self.assertIsNone(self.owner.acquire(A))
        self.register(2, B)
        a, b = self.install(A, host="api.example.com"), self.install(B)
        self.assertEqual(a.revision.decision_epoch, b.revision.decision_epoch)
        self.assertNotEqual(a.payload, b.payload)
        self.assertEqual(self.owner.acquire(A), a)
        self.install(A, epoch=2)
        self.assertEqual(self.owner.acquire(B), b)
        self.assert_error("subject_generation_mismatch", self.owner.readback,
                          subject.Target(A.subject_key, B.subject_generation))
        self.assert_error("subject_generation_mismatch", self.owner.commit, A, b.revision)

    def test_duplicate_register_preserves_active_pending_and_abort_state(self):
        self.register(1)
        active = self.install()
        pending = snapshot(A, 2, "api.example.com")
        self.owner.prepare(A, pending.revision, pending.payload)
        original = self.owner._entries[A.subject_key].receiver
        self.assertEqual(self.register(2)["result"]["outcome"], "already_registered")
        self.assertIs(self.owner._entries[A.subject_key].receiver, original)
        self.assertEqual(self.owner.acquire(A), active)
        self.owner.commit(A, pending.revision)
        self.owner.abort(A, snapshot(A, 3).revision)
        self.register(3)
        self.assert_error("revision_rejected", self.owner.prepare, A,
                          snapshot(A, 3).revision, snapshot(A, 3).payload)
        self.assertEqual(self.owner.readback(A), pending.revision)

    def test_capacity_conflict_and_release(self):
        self.register(1)
        active = self.install()
        self.register(2, B)
        other = subject.Target("s-c", "generation-c")
        self.assertEqual(self.register(3, other)["result"]["outcome"], "subject_capacity_exhausted")
        self.assertEqual(self.register(4, subject.Target("s-a", "new"))["result"]["outcome"], "subject_conflict")
        self.assertEqual(self.owner.acquire(A), active)
        unloaded = self.owner.lifecycle("unload", command(5, A))
        self.assertEqual(unloaded["result"]["unload"], {
            "routingRevoked": True, "transportObligations": "none", "completion": "terminal"})
        self.assertEqual(self.register(6, other)["result"]["outcome"], "registered")
        self.assertEqual(len(self.owner._entries), 2)
        self.assertEqual(self.owner.lifecycle("unload", command(7, A))["result"]["outcome"], "already_absent")

    def test_sequence_replay_conflict_gap_and_exact_bytes(self):
        raw = command(1)
        first = self.owner.lifecycle("register", raw)
        self.assertEqual(self.owner.lifecycle("register", raw), first)
        first["result"]["outcome"] = "tampered"
        self.assertEqual(self.owner.lifecycle("register", raw)["result"]["outcome"], "registered")
        self.assert_error("lifecycle_sequence_conflict", self.owner.lifecycle, "register", raw + b" ")
        self.assert_error("lifecycle_sequence_conflict", self.owner.lifecycle, "unload", raw)
        self.assert_error("sequence_gap", self.owner.lifecycle, "register", command(3, B))
        self.owner.lifecycle("unload", command(2))
        self.assert_error("stale_lifecycle_sequence", self.owner.lifecycle, "register", raw)
        new = subject.Target("s-a", "new-generation")
        self.register(3, new)
        self.assert_error("stale_lifecycle_sequence", self.owner.lifecycle, "unload", command(2))
        for operation in (self.owner.commit, self.owner.abort):
            self.assert_error("subject_generation_mismatch", operation, A, snapshot().revision)
        self.assert_error("subject_generation_mismatch", self.owner.readback, A)
        self.assertEqual(self.owner.lifecycle("unload", command(4))["result"]["outcome"], "subject_generation_mismatch")
        self.assertIsNone(self.owner.readback(new))

    def test_invalid_target_consumes_sequence_but_outer_errors_do_not(self):
        invalid_targets = (None, [], 1, {}, {**wire(A), "extra": "secret"},
                           {"subjectKey": "", "subjectGeneration": "x"},
                           {"subjectKey": "x" * 257, "subjectGeneration": "x"},
                           {"subjectKey": "x", "subjectGeneration": "x" * 129},
                           {"subjectKey": "\ud800", "subjectGeneration": "x"},
                           {"subjectKey": "x", "subjectGeneration": "\udfff"})
        for sequence, target in enumerate(invalid_targets, 1):
            raw = command(sequence, target)
            receipt = self.owner.lifecycle("register", raw)
            self.assertEqual(receipt["result"], {"httpStatus": 400, "outcome": "invalid_target", "unload": None})
            self.assertEqual(self.owner.lifecycle("register", raw), receipt)
            self.assertNotIn("secret", json.dumps(receipt))
        previous = self.owner.lifecycle_status(CONTROL)
        malformed = [b"{", b"[]", command(11, extra=1), command(True), command(1.0),
                     command(0), command(-1), command(2**63), command(1, control=""),
                     command(1, control="\ud800"), b'{"controlGeneration":"a","controlGeneration":"a"}',
                     command(11).replace(b'"s-a"', b'NaN'), command(11).decode().encode("utf-16")]
        for raw in malformed:
            self.assert_error("malformed_request", self.owner.lifecycle, "register", raw)
            self.assertEqual(self.owner.lifecycle_status(CONTROL), previous)
        self.assert_error("control_generation_mismatch", self.owner.lifecycle, "register", command(11, control="old-session"))
        self.assertEqual(self.owner.lifecycle_status(CONTROL), previous)

    def test_lifecycle_cap_and_fixed_status_near_boundary(self):
        raw = command(1, {"secret": "x"})
        raw += b" " * (8192 - len(raw))
        receipt = self.owner.lifecycle("register", raw)
        self.assertEqual(receipt["result"]["outcome"], "invalid_target")
        self.assertEqual(len(self.owner._last.raw_body), 8192)
        status = self.owner.lifecycle_status(CONTROL)
        self.assertLessEqual(len(json.dumps(status).encode()), 4096)
        self.assertNotIn("secret", json.dumps(status))
        self.assert_error("request_too_large", self.owner.lifecycle, "register", command(2) + b" " * 8192)
        self.assertEqual(self.owner.lifecycle_status(CONTROL), status)
        self.assertNotIn("secret", repr(self.owner._last))

    def test_receiver_failure_fences_only_subject_and_failed_close_is_retryable(self):
        self.register(1)
        self.register(2, B)
        a_receiver = self.owner._entries[A.subject_key].receiver
        with mock.patch.object(a_receiver, "prepare", side_effect=RuntimeError("secret")):
            self.assert_error("subject_failed", self.owner.prepare, A, snapshot().revision, snapshot().payload)
        self.assertEqual(self.register(3)["result"]["outcome"], "subject_failed")
        for operation in (self.owner.readback, self.owner.acquire):
            self.assert_error("subject_failed", operation, A)
        for operation in (self.owner.commit, self.owner.abort):
            self.assert_error("subject_failed", operation, A, snapshot().revision)
        b = self.install(B)
        with mock.patch.object(a_receiver, "close", side_effect=RuntimeError("secret")) as close:
            raw = command(4)
            receipt = self.owner.lifecycle("unload", raw)
            self.assertEqual(receipt["result"], {"httpStatus": 503, "outcome": "subject_failed", "unload": {
                "routingRevoked": True, "transportObligations": "none", "completion": "fenced"}})
            self.assertEqual(self.owner.lifecycle("unload", raw), receipt)
            close.assert_called_once()
            self.assertEqual(self.register(5, subject.Target("s-c", "new"))["result"]["outcome"], "subject_capacity_exhausted")
        self.assertEqual(self.owner.lifecycle("unload", command(6))["result"]["outcome"], "unloaded")
        self.assertEqual(self.owner.acquire(B), b)
        self.assertEqual(self.register(7, subject.Target("s-a", "new"))["result"]["outcome"], "registered")

    def test_receiver_construction_failure_consumes_without_half_entry(self):
        with mock.patch.object(subject, "InstallationReceiver", side_effect=RuntimeError("secret")):
            self.assertEqual(self.register(1)["result"]["outcome"], "subject_failed")
        self.assertEqual(self.owner._entries, {})
        self.assertEqual(self.register(2)["result"]["outcome"], "registered")

    def test_revision_error_and_invalid_snapshot_do_not_fence_subject(self):
        self.register(1)
        self.assert_error("revision_rejected", self.owner.commit, A, snapshot().revision)
        raw = b'{"secret":"invalid"}'
        bad = replace(snapshot().revision, digest=hashlib.sha256(raw).hexdigest())
        self.assert_error("revision_rejected", self.owner.prepare, A, bad, raw)
        self.assert_error("revision_rejected", self.owner.prepare, A, snapshot().revision, b"wrong digest")
        self.assertFalse(self.owner._entries[A.subject_key].failed)
        self.install()

    def test_publication_failure_is_process_terminal_even_after_assignment(self):
        self.register(1)
        self.register(2, B)
        original = subject.SubjectRevisionOwner.__setattr__
        def partial(owner, name, value):
            original(owner, name, value)
            if owner is self.owner and name == "_last":
                raise RuntimeError("secret")
        raw = command(3)
        with mock.patch.object(subject.SubjectRevisionOwner, "__setattr__", partial):
            self.assert_error("internal_error", self.owner.lifecycle, "register", raw)
        for operation, args in ((self.owner.lifecycle, ("register", raw)),
                                (self.owner.lifecycle_status, (CONTROL,)),
                                (self.owner.readback, (B,)), (self.owner.acquire, (B,)),
                                (self.owner.prepare, (B, snapshot(B).revision, snapshot(B).payload))):
            self.assert_error("internal_error", operation, *args)

    def test_map_publication_failure_is_terminal(self):
        class BrokenMap(dict):
            def __setitem__(self, key, value):
                super().__setitem__(key, value)
                raise RuntimeError("secret")
        self.owner._entries = BrokenMap()
        self.assert_error("internal_error", self.owner.lifecycle, "register", command(1))
        self.assert_error("internal_error", self.owner.lifecycle_status, CONTROL)

    def test_close_tries_all_and_reports_failure_then_can_retry(self):
        self.register(1)
        self.register(2, B)
        first = self.owner._entries[A.subject_key].receiver
        second = self.owner._entries[B.subject_key].receiver
        with mock.patch.object(first, "close", side_effect=RuntimeError("secret")), \
                mock.patch.object(second, "close", wraps=second.close) as other:
            self.assert_error("internal_error", self.owner.close)
            other.assert_called_once()
            self.assertEqual(len(self.owner._entries), 1)
        self.owner.close()
        self.owner.close()
        self.assertEqual(self.owner._entries, {})
        self.assert_error("internal_error", self.owner.readback, A)

    def test_churn_keeps_no_receiver_or_history(self):
        for index in range(10000):
            target = subject.Target(f"s-{index}", f"g-{index}")
            self.owner.lifecycle("register", command(2 * index + 1, target))
            reference = weakref.ref(self.owner._entries[target.subject_key].receiver)
            self.owner.lifecycle("unload", command(2 * index + 2, target))
            self.assertLessEqual(len(self.owner._entries), 2)
        gc.collect()
        self.assertIsNone(reference())
        self.assertEqual(self.owner._entries, {})
        self.assertEqual(self.owner._last.sequence, 20000)
        self.assertLessEqual(len(self.owner._last.raw_body), 8192)
        self.assertEqual(set(self.owner.__dict__), {"_control", "_max_subjects", "_max_snapshot_bytes", "_entries", "_last", "_operation_lock", "_terminal", "_closed"})

    def test_abort_history_limit_does_not_consume_other_subject_budget(self):
        self.register(1)
        self.register(2, B)
        for epoch in range(1, 1025):
            self.owner.abort(A, snapshot(A, epoch).revision)
        rejected = snapshot(A, 1025)
        self.assert_error("revision_rejected", self.owner.prepare, A, rejected.revision, rejected.payload)
        self.assert_error("revision_rejected", self.owner.abort, A, rejected.revision)
        self.install(B)
        self.owner.lifecycle("unload", command(3))
        new = subject.Target(A.subject_key, "replacement")
        self.register(4, new)
        self.install(new)

    def test_operation_mutex_orders_revision_before_unload_and_unload_before_revision(self):
        for operation in ("prepare", "commit"):
            with self.subTest(operation=operation):
                owner = make_owner()
                owner.lifecycle("register", command(1))
                item = snapshot()
                if operation == "commit":
                    owner.prepare(A, item.revision, item.payload)
                entry_receiver = owner._entries[A.subject_key].receiver
                started, release, unloaded = threading.Event(), threading.Event(), threading.Event()
                original = getattr(entry_receiver, operation)
                errors = []
                def blocked(*args):
                    started.set()
                    if not release.wait(3):
                        raise AssertionError("release timeout")
                    return original(*args)
                def revise():
                    try:
                        getattr(owner, operation)(A, item.revision, *([item.payload] if operation == "prepare" else []))
                    except Exception as exc:
                        errors.append(exc)
                def unload():
                    owner.lifecycle("unload", command(2))
                    unloaded.set()
                with mock.patch.object(entry_receiver, operation, side_effect=blocked):
                    first, second = threading.Thread(target=revise), threading.Thread(target=unload)
                    first.start()
                    self.assertTrue(started.wait(3))
                    second.start()
                    self.assertFalse(unloaded.wait(0.02))
                    release.set()
                    first.join(3)
                    second.join(3)
                self.assertEqual(errors, [])
                self.assertTrue(unloaded.is_set())
                self.assert_error("subject_unknown", owner.acquire, A)
                args = (A, item.revision, *([item.payload] if operation == "prepare" else []))
                self.assert_error("subject_unknown", getattr(owner, operation), *args)
                owner.close()

    def test_unload_holds_mutex_until_close_then_waiting_revision_is_rejected(self):
        for operation in ("prepare", "commit"):
            with self.subTest(operation=operation):
                owner = make_owner()
                owner.lifecycle("register", command(1))
                item = snapshot()
                owner.prepare(A, item.revision, item.payload)
                underlying = owner._entries[A.subject_key].receiver
                close_started, release_close, revision_finished = (threading.Event() for _ in range(3))
                original_close = underlying.close
                errors = []
                def close():
                    close_started.set()
                    if not release_close.wait(3):
                        raise AssertionError("release timeout")
                    original_close()
                def revise():
                    try:
                        getattr(owner, operation)(A, item.revision, *([item.payload] if operation == "prepare" else []))
                    except subject.SubjectError as error:
                        errors.append(error.outcome)
                    finally:
                        revision_finished.set()
                with mock.patch.object(underlying, "close", side_effect=close):
                    unload = threading.Thread(target=owner.lifecycle, args=("unload", command(2)))
                    revision = threading.Thread(target=revise)
                    unload.start()
                    self.assertTrue(close_started.wait(3))
                    revision.start()
                    self.assertFalse(revision_finished.wait(0.02))
                    release_close.set()
                    unload.join(3)
                    revision.join(3)
                self.assertEqual(errors, ["subject_unknown"])
                self.assertEqual(owner._entries, {})
                owner.close()

    def test_failed_flag_publication_exception_is_terminal(self):
        self.register(1)
        self.register(2, B)
        underlying = self.owner._entries[A.subject_key].receiver
        original = subject._Entry.__setattr__
        def fail_flag(entry, name, value):
            if name == "failed":
                raise RuntimeError("secret")
            original(entry, name, value)
        with mock.patch.object(underlying, "readback", side_effect=RuntimeError("secret")), \
                mock.patch.object(subject._Entry, "__setattr__", fail_flag):
            self.assert_error("internal_error", self.owner.readback, A)
        self.assert_error("internal_error", self.owner.readback, B)
        self.assert_error("internal_error", self.owner.lifecycle_status, CONTROL)

    def test_max_sequence_no_wrap(self):
        self.register(1)
        self.owner._last = replace(self.owner._last, sequence=subject.MAX_SEQUENCE - 1)
        raw = command(subject.MAX_SEQUENCE)
        receipt = self.owner.lifecycle("register", raw)
        self.assertEqual(self.owner.lifecycle("register", raw), receipt)
        self.assert_error("malformed_request", self.owner.lifecycle, "register", command(2**63))
        self.assert_error("stale_lifecycle_sequence", self.owner.lifecycle, "register", command(1))


class SubjectBoundsTest(unittest.TestCase):
    def test_checked_envelope(self):
        for name in ("max_subjects", "max_snapshot_bytes", "retained_payload_budget"):
            for invalid in (True, False, 0, -1, 1.0, "1", sys.maxsize + 1):
                with self.subTest(name=name, invalid=invalid), self.assertRaises(ValueError):
                    make_owner(**{name: invalid})
        for values in ({"retained_payload_budget": 8191}, {"max_subjects": sys.maxsize},
                       {"max_snapshot_bytes": sys.maxsize}):
            with self.assertRaises(ValueError):
                make_owner(**values)
        for control in ("", "x" * 129, "\ud800", 1):
            with self.assertRaises(ValueError):
                subject.SubjectRevisionOwner(control, max_subjects=1, max_snapshot_bytes=1, retained_payload_budget=2)
        with self.assertRaises(ValueError):
            subject.request_body_limit(sys.maxsize)
        make_owner().close()

    def test_maximum_unicode_and_control_escape_metadata_fit(self):
        for text in ("\U0001f680", "\x00"):
            target = subject.Target(text * 256, text * 128)
            revision = receiver.Revision(text * 128, text * 128, subject.MAX_SEQUENCE,
                                         subject.MAX_SEQUENCE, subject.MAX_SEQUENCE, "f" * 64)
            for ensure_ascii in (False, True):
                raw = json.dumps({"controlGeneration": text * 128,
                                  "lifecycleSequence": subject.MAX_SEQUENCE, "target": wire(target)},
                                 ensure_ascii=ensure_ascii, separators=(",", ":")).encode()
                self.assertLessEqual(len(raw), 8192)
                from revision_ipc import _to_wire
                metadata = json.dumps({"target": wire(target), "revision": _to_wire(revision), "payload": ""},
                                      ensure_ascii=ensure_ascii, separators=(",", ":")).encode()
                self.assertLessEqual(len(metadata), 8192)
                self.assertLessEqual(len(json.dumps({"revision": _to_wire(revision)}, ensure_ascii=ensure_ascii).encode()), 4096)
                self.assertEqual(subject.Target.from_wire(json.loads(raw)["target"]), target)

    def test_command_digest_golden(self):
        raw = b'{"controlGeneration":"control-test","lifecycleSequence":1,"target":{"subjectKey":"s-sandbox-a","subjectGeneration":"subject-a-generation-1"}}'
        self.assertEqual(subject.command_digest("register", raw),
                         "e16efb0460b8a00399e49ee90c7cc777fdabda7beaed808519927351da92a32e")
        self.assertNotEqual(subject.command_digest("register", raw), subject.command_digest("unload", raw))
        self.assertNotEqual(subject.command_digest("register", raw), subject.command_digest("register", raw + b" "))


if __name__ == "__main__":
    unittest.main()
