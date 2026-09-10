# Copyright 2026 Alibaba Group Holding Ltd.
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

"""Receiver state transitions are independent of IPC and TLS hooks."""

import dataclasses
import hashlib
import importlib.util
import json
import sys
import threading
import unittest
from pathlib import Path

PATH = Path(__file__).resolve().parents[1] / "mitmscripts" / "revision_receiver.py"
SPEC = importlib.util.spec_from_file_location("revision_receiver", PATH)
receiver = importlib.util.module_from_spec(SPEC)
sys.modules[SPEC.name] = receiver
SPEC.loader.exec_module(receiver)


class RevisionReceiverTest(unittest.TestCase):
    def setUp(self):
        self.validated = []

        def validate(snapshot):
            value = json.loads(snapshot.payload)
            if value["revision"] != snapshot.revision.vault_revision:
                raise ValueError("secret-bearing validation detail")
            self.validated.append(snapshot.revision)

        self.validate = validate
        self.store = receiver.Receiver(
            "control-a", "subject-a", validate, max_snapshot_bytes=1024
        )

    def candidate(self, epoch=1, vault_revision=1, **changes):
        payload = json.dumps(
            {"revision": vault_revision, "secret": "never-log-me"}
        ).encode()
        fields = {
            "control_generation": "control-a",
            "subject_generation": "subject-a",
            "decision_epoch": epoch,
            "vault_revision": vault_revision,
            "policy_epoch": 1,
            "digest": hashlib.sha256(payload).hexdigest(),
        }
        fields.update(changes)
        return receiver.Revision(**fields), payload

    def test_prepare_is_inert_commit_ack_matches_readback(self):
        key, payload = self.candidate()
        self.assertIsNone(self.store.readback())
        self.assertIsNone(self.store.acquire())
        self.assertEqual(self.store.prepare(key, payload), key)
        self.assertIsNone(self.store.readback())
        self.assertEqual(self.store.prepare(key, payload), key)
        self.assertEqual(len(self.validated), 1)
        self.assertEqual(self.store.commit(key), key)
        self.assertEqual(self.store.readback(), key)
        self.assertEqual(self.store.commit(key), key)
        self.assertEqual(self.store.acquire().payload, payload)

    def test_abort_never_reverts_active_and_consumes_epoch(self):
        one, payload = self.candidate()
        self.store.prepare(one, payload)
        self.store.commit(one)
        two, payload = self.candidate(2, 2)
        self.store.prepare(two, payload)
        self.assertEqual(self.store.abort(two), two)
        self.assertEqual(self.store.abort(two), two)
        self.assertEqual(self.store.readback(), one)
        with self.assertRaises(receiver.RevisionError):
            self.store.commit(two)
        with self.assertRaises(receiver.RevisionError):
            self.store.prepare(two, payload)
        with self.assertRaises(receiver.RevisionError):
            self.store.abort(one)
        self.assertEqual(self.store.readback(), one)

    def test_prior_abort_retries_survive_later_transitions(self):
        one, data = self.candidate()
        self.store.prepare(one, data)
        self.store.abort(one)
        two, data = self.candidate(2, 2)
        self.store.prepare(two, data)
        self.assertEqual(self.store.abort(one), one)
        self.store.commit(two)
        three, data = self.candidate(3, 3)
        self.store.prepare(three, data)
        self.store.abort(three)
        four, data = self.candidate(4, 4)
        self.store.prepare(four, data)
        for retired in (one, three, one):
            self.assertEqual(self.store.abort(retired), retired)
        for changes in ({"digest": "0" * 64}, {"policy_epoch": 2}):
            with self.assertRaises(receiver.RevisionError):
                self.store.abort(dataclasses.replace(one, **changes))
        self.assertEqual(self.store.readback(), two)
        self.store.commit(four)  # retries did not discard the pending snapshot
        self.assertEqual(self.store.readback(), four)

    def test_abort_history_capacity_preserves_retries_and_pending_reservation(self):
        store = receiver.Receiver(
            "control-a",
            "subject-a",
            self.validate,
            max_snapshot_bytes=1024,
            max_abort_records=2,
        )
        one, data = self.candidate()
        store.prepare(one, data)
        two, _ = self.candidate(2, 2)
        store.abort(two)
        three, data = self.candidate(3, 3)
        with self.assertRaisesRegex(receiver.RevisionError, "capacity"):
            store.abort(three)  # reserve the last slot for the pending tuple
        store.abort(one)
        validated = len(self.validated)
        with self.assertRaisesRegex(receiver.RevisionError, "capacity"):
            store.prepare(three, data)
        self.assertEqual(len(self.validated), validated)
        for retired in (one, two, one):
            self.assertEqual(store.abort(retired), retired)
        self.assertIsNone(store.readback())
        store.close()
        with self.assertRaises(receiver.RevisionError):
            store.abort(one)

    def test_full_abort_history_preserves_active_state_and_retries(self):
        store = receiver.Receiver(
            "control-a",
            "subject-a",
            self.validate,
            max_snapshot_bytes=1024,
            max_abort_records=1,
        )
        one, data = self.candidate()
        store.prepare(one, data)
        store.commit(one)
        two, _ = self.candidate(2, 2)
        store.abort(two)
        self.assertEqual(store.prepare(one, data), one)
        self.assertEqual(store.commit(one), one)
        self.assertEqual(store.acquire().revision, one)
        three, _ = self.candidate(3, 3)
        with self.assertRaisesRegex(receiver.RevisionError, "capacity"):
            store.abort(three)
        self.assertEqual(store.abort(two), two)
        self.assertEqual(store.readback(), one)

    def test_abort_history_budget_requires_positive_integer(self):
        for limit in (True, False, 0, -1, 1.5, "2", None):
            with self.subTest(limit=limit), self.assertRaises(ValueError):
                receiver.Receiver(
                    "control-a",
                    "subject-a",
                    self.validate,
                    max_snapshot_bytes=1024,
                    max_abort_records=limit,
                )

    def test_identity_conflict_stale_and_delete_recreate_aba(self):
        for field in ("control_generation", "subject_generation"):
            key, payload = self.candidate(**{field: "old-generation"})
            with self.assertRaises(receiver.RevisionError):
                self.store.prepare(key, payload)
        one, data = self.candidate()
        self.store.prepare(one, data)
        changed, _ = self.candidate(policy_epoch=2)
        with self.assertRaises(receiver.RevisionError):
            self.store.prepare(changed, data)
        self.store.commit(one)
        two, data = self.candidate(2, 0)  # empty tombstone
        self.store.prepare(two, data)
        self.store.commit(two)
        three, data = self.candidate(3, 1)  # a new public Vault lifetime
        self.store.prepare(three, data)
        self.store.commit(three)
        with self.assertRaises(receiver.RevisionError):
            self.store.commit(one)
        self.assertEqual(self.store.readback(), three)

    def test_snapshot_and_old_request_handle_are_immutable(self):
        one, payload = self.candidate()
        self.store.prepare(one, payload)
        self.store.commit(one)
        pinned = self.store.acquire()
        with self.assertRaises(dataclasses.FrozenInstanceError):
            pinned.payload = b"changed"
        with self.assertRaises(dataclasses.FrozenInstanceError):
            pinned.revision.vault_revision = 42
        two, payload = self.candidate(2, 2)
        self.store.prepare(two, payload)
        self.store.commit(two)
        self.assertEqual(pinned.revision, one)
        self.assertNotIn("never-log-me", repr(pinned))
        self.assertNotIn("never-log-me", repr(self.store))

    def test_size_digest_and_validation_errors_preserve_state(self):
        one, payload = self.candidate()
        for bad_payload in (b"changed", bytearray(payload), b"x" * 1025):
            with self.assertRaises(receiver.RevisionError):
                self.store.prepare(one, bad_payload)
        invalid = dataclasses.replace(one, vault_revision=2)
        with self.assertRaises(receiver.RevisionError) as caught:
            self.store.prepare(invalid, payload)
        self.assertNotIn("secret-bearing", str(caught.exception))
        self.assertIsNone(caught.exception.__cause__)
        self.assertIsNone(caught.exception.__context__)
        self.assertIsNone(self.store.acquire())
        self.store.prepare(one, payload)  # rejected validation never installed
        self.store.commit(one)

    def test_close_fences_inflight_validation_without_waiting(self):
        entered, release = threading.Event(), threading.Event()
        outcomes = []

        def blocked_validate(snapshot):
            entered.set()
            if not release.wait(2):
                raise RuntimeError("test validation timeout")
            self.validate(snapshot)

        store = receiver.Receiver(
            "control-a", "subject-a", blocked_validate, max_snapshot_bytes=1024
        )
        key, payload = self.candidate()

        def worker():
            try:
                store.prepare(key, payload)
            except receiver.RevisionError as exc:
                outcomes.append(str(exc))

        thread = threading.Thread(target=worker)
        thread.start()
        try:
            self.assertTrue(entered.wait(2))
            store.close()
        finally:
            release.set()
            thread.join(3)
        self.assertFalse(thread.is_alive())
        self.assertEqual(outcomes, ["receiver closed"])
        store.close()
        for call in (
            store.acquire,
            store.readback,
            lambda: store.commit(key),
            lambda: store.abort(key),
        ):
            with self.assertRaises(receiver.RevisionError):
                call()

    def test_concurrent_prepare_cannot_replace_pending_revision(self):
        entered, release = threading.Event(), threading.Event()
        results = []

        def slow(snapshot):
            if snapshot.revision.decision_epoch == 1:
                entered.set()
                release.wait(2)

        store = receiver.Receiver(
            "control-a", "subject-a", slow, max_snapshot_bytes=1024
        )
        one, data = self.candidate()
        two, other = self.candidate(2, 2)

        def worker():
            try:
                results.append(store.prepare(one, data))
            except receiver.RevisionError:
                results.append("conflict")

        thread = threading.Thread(target=worker)
        thread.start()
        self.assertTrue(entered.wait(2))
        # A concurrent valid prepare wins; the slow one must recheck state.
        try:
            self.assertEqual(store.prepare(two, other), two)
        finally:
            release.set()
            thread.join(3)
        self.assertEqual(results, ["conflict"])
        self.assertEqual(store.commit(two), two)

    def test_revision_fields_reject_ambiguous_wire_values(self):
        for changes in (
            {"decision_epoch": True},
            {"decision_epoch": 0},
            {"decision_epoch": 2**63},
            {"vault_revision": -1},
            {"policy_epoch": 1.0},
            {"digest": "bad"},
            {"control_generation": ""},
        ):
            with self.subTest(changes=changes), self.assertRaises(ValueError):
                self.candidate(**changes)

    def test_abort_fences_prepare_still_validating(self):
        entered, release = threading.Event(), threading.Event()
        outcomes = []

        def slow(snapshot):
            entered.set()
            release.wait(2)

        store = receiver.Receiver(
            "control-a", "subject-a", slow, max_snapshot_bytes=1024
        )
        key, payload = self.candidate()

        def worker():
            try:
                store.prepare(key, payload)
                outcomes.append("prepared")
            except receiver.RevisionError:
                outcomes.append("rejected")

        thread = threading.Thread(target=worker)
        thread.start()
        try:
            self.assertTrue(entered.wait(2))
            self.assertEqual(store.abort(key), key)
        finally:
            release.set()
            thread.join(3)
        self.assertEqual(outcomes, ["rejected"])
        self.assertIsNone(store.acquire())

    def test_lost_commit_ack_is_resolved_without_resending_payload(self):
        one, payload = self.candidate()
        self.store.prepare(one, payload)
        self.store.commit(one)  # simulate dropping the response
        self.assertEqual(self.store.readback(), one)
        self.assertFalse(hasattr(self.store.readback(), "payload"))
        two, other = self.candidate(2, 2)
        self.store.prepare(two, other)
        self.assertEqual(self.store.commit(one), one)  # late retry is inert
        self.assertEqual(self.store.commit(two), two)

    def test_validator_must_explicitly_succeed_with_none(self):
        key, payload = self.candidate()
        for result in (False, True, "validation-failed"):
            store = receiver.Receiver(
                "control-a",
                "subject-a",
                lambda snapshot, result=result: result,
                max_snapshot_bytes=1024,
            )
            with self.assertRaises(receiver.RevisionError):
                store.prepare(key, payload)
            self.assertIsNone(store.acquire())

    def test_future_abort_does_not_cancel_another_prepared_tuple(self):
        one, payload = self.candidate()
        self.store.prepare(one, payload)
        self.store.commit(one)
        three, payload = self.candidate(3, 2)
        self.store.prepare(three, payload)
        four, later = self.candidate(4, 3)
        self.store.abort(four)
        self.assertEqual(self.store.commit(three), three)
        with self.assertRaises(receiver.RevisionError):
            self.store.prepare(four, later)
        self.assertEqual(self.store.readback(), three)

    def test_failed_validation_and_unknown_abort_preserve_active(self):
        one, payload = self.candidate()
        self.store.prepare(one, payload)
        self.store.commit(one)
        two = dataclasses.replace(one, decision_epoch=2, vault_revision=2)
        with self.assertRaises(receiver.RevisionError):
            self.store.prepare(two, payload)
        self.assertEqual(self.store.abort(two), two)
        self.assertEqual(self.store.readback(), one)
        self.assertEqual(self.store.acquire().payload, payload)

    def test_same_epoch_different_digest_and_generation_commands_fail(self):
        one, payload = self.candidate()
        self.store.prepare(one, payload)
        self.store.commit(one)
        different_payload = payload + b" "
        different = dataclasses.replace(
            one, digest=hashlib.sha256(different_payload).hexdigest()
        )
        with self.assertRaises(receiver.RevisionError):
            self.store.prepare(different, different_payload)
        foreign = dataclasses.replace(one, subject_generation="foreign")
        for call in (
            lambda: self.store.commit(foreign),
            lambda: self.store.abort(foreign),
            lambda: self.store.prepare(None, payload),
        ):
            with self.assertRaises(receiver.RevisionError):
                call()
        self.assertEqual(self.store.readback(), one)
