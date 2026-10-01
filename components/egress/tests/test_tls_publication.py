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

"""Joint revision ownership, failure atomicity and retirement semantics."""

import hashlib
import json
import traceback
import unittest
from dataclasses import replace
from unittest.mock import patch

import test_tls_registry as fixtures

publication = fixtures.load("revision_publication")
receiver = fixtures.receiver
registry_module = fixtures.registry_module


class RevisionPublicationTest(unittest.TestCase):
    identity = fixtures.TLSRegistryTest.identity
    snapshot = fixtures.TLSRegistryTest.snapshot
    admit = fixtures.TLSRegistryTest.admit

    def owner(self, **kwargs):
        return publication.RevisionPublisher(
            *self.identity, max_snapshot_bytes=100_000, capacity=8, **kwargs,
        )

    def install(self, owner, snapshot):
        owner.prepare(snapshot.revision, snapshot.payload)
        return owner.commit(snapshot.revision)

    def state(self, owner):
        active, registry = owner._receiver, owner.registry
        return (
            active._active, active._pending, active._serial, active._highest_epoch,
            active._aborted.copy(), owner._pending_view, registry._view,
            registry._generation, registry._request_fenced.copy(),
            registry._connection_deadlines.copy(), registry._request_deadlines.copy(),
        )

    def test_prepare_is_inert_and_commit_publishes_both_views(self):
        owner = self.owner()
        first = self.snapshot()
        self.assertIsNone(owner.readback())
        self.assertEqual(self.admit(owner.registry).reason, "snapshot_missing")
        self.assertEqual(owner.prepare(first.revision, first.payload), first.revision)
        self.assertIsNone(owner.acquire())
        self.assertEqual(self.admit(owner.registry).reason, "snapshot_missing")
        with patch.object(publication, "compile_view", side_effect=AssertionError):
            self.assertEqual(owner.commit(first.revision), ())
        self.assertEqual(owner.readback(), first.revision)
        token = self.admit(owner.registry).token
        request = owner.registry.acquire_request(token)
        self.assertEqual(request.action, "allow")
        self.assertEqual(request.snapshot, first)
        self.assertIs(request.snapshot, owner.acquire())

    def test_empty_commit_is_authoritative_and_fences_late_admissions(self):
        owner = self.owner()
        self.install(owner, self.snapshot())
        empty = self.snapshot(epoch=2, host=None)
        owner.prepare(empty.revision, empty.payload)
        token = self.admit(owner.registry).token
        request = owner.registry.acquire_request(token)
        with patch.object(registry_module.time, "monotonic", return_value=50):
            self.assertEqual(owner.commit(empty.revision), (token,))
        self.assertEqual(owner.acquire(), empty)
        self.assertEqual(self.admit(owner.registry).action, "passthrough")
        self.assertEqual(owner.registry.acquire_request(token).reason, "connection_fenced")
        self.assertEqual(owner.registry._connection_deadlines, {token.serial: 80})
        self.assertEqual(owner.registry._request_deadlines, {request.handle.serial: 80})

    def test_active_and_pending_retries_preserve_newer_candidate_and_deadlines(self):
        owner = self.owner()
        first = self.snapshot(secret="old-secret")
        self.install(owner, first)
        token = self.admit(owner.registry).token
        old = owner.registry.acquire_request(token)
        second = self.snapshot(epoch=2, secret="new-secret")
        with patch.object(registry_module.time, "monotonic", return_value=10):
            self.install(owner, second)
        third = self.snapshot(epoch=3)
        owner.prepare(third.revision, third.payload)
        before = self.state(owner)
        with patch.object(publication, "compile_view", side_effect=AssertionError), \
                patch.object(registry_module.time, "monotonic", side_effect=AssertionError):
            self.assertEqual(owner.prepare(second.revision, second.payload), second.revision)
            self.assertEqual(owner.prepare(third.revision, third.payload), third.revision)
            self.assertEqual(owner.commit(second.revision), ())
        self.assertEqual(self.state(owner), before)
        with patch.object(registry_module.time, "monotonic", return_value=20):
            owner.commit(third.revision)
        self.assertEqual(owner.registry._request_deadlines, {old.handle.serial: 40})
        self.assertEqual(old.snapshot, first)
        self.assertEqual(owner.registry.acquire_request(token).snapshot, third)

    def test_aborts_preserve_unrelated_candidate_and_exact_history(self):
        owner = self.owner()
        first, second, future = (self.snapshot(epoch=epoch) for epoch in (1, 2, 3))
        self.install(owner, first)
        owner.prepare(second.revision, second.payload)
        owner.abort(future.revision)
        self.assertEqual(owner.abort(future.revision), future.revision)
        owner.commit(second.revision)
        self.assertEqual(owner.readback(), second.revision)
        for candidate in (first, future):
            with self.assertRaises(receiver.RevisionError):
                owner.prepare(candidate.revision, candidate.payload)
            with self.assertRaises(receiver.RevisionError):
                owner.commit(candidate.revision)
        fourth = self.snapshot(epoch=4)
        owner.prepare(fourth.revision, fourth.payload)
        owner.abort(fourth.revision)
        self.assertIsNone(owner._pending_view)
        self.assertEqual(owner.abort(future.revision), future.revision)
        with self.assertRaises(receiver.RevisionError):
            owner.abort(second.revision)

    def test_abort_budget_reserves_prepared_candidate(self):
        owner = self.owner(max_abort_records=1)
        first, future = self.snapshot(), self.snapshot(epoch=2)
        owner.prepare(first.revision, first.payload)
        with self.assertRaises(receiver.RevisionError):
            owner.abort(future.revision)
        owner.abort(first.revision)
        self.assertIsNone(owner._pending_view)
        self.assertEqual(owner.abort(first.revision), first.revision)
        with self.assertRaises(receiver.RevisionError):
            owner.prepare(future.revision, future.payload)

    def test_rejected_prepare_and_commit_preserve_complete_state(self):
        owner = self.owner()
        first = self.snapshot()
        self.install(owner, first)
        pending = self.snapshot(epoch=2)
        owner.prepare(pending.revision, pending.payload)
        before = self.state(owner)
        candidates = (
            self.snapshot(epoch=3), self.snapshot(epoch=2, secret="conflict"),
            self.snapshot(epoch=2, identity=("other-control", "subject-a")),
            self.snapshot(epoch=2, identity=("control-a", "other-subject")),
        )
        for candidate in candidates:
            with self.subTest(revision=candidate.revision):
                for operation in (
                    lambda candidate=candidate: owner.prepare(candidate.revision, candidate.payload),
                    lambda candidate=candidate: owner.commit(candidate.revision),
                ):
                    with self.assertRaises(receiver.RevisionError):
                        operation()
                    self.assertEqual(self.state(owner), before)
        for payload in (bytearray(pending.payload), b"wrong", b"x" * 100_001):
            with self.assertRaises(receiver.RevisionError):
                owner.prepare(pending.revision, payload)
            self.assertEqual(self.state(owner), before)
        conflict = replace(pending.revision, policy_epoch=99)
        with self.assertRaises(receiver.RevisionError):
            owner.commit(conflict)
        self.assertEqual(self.state(owner), before)

    def test_invalid_compilation_is_sanitized_and_does_not_stage(self):
        owner = self.owner()
        first = self.snapshot()
        self.install(owner, first)
        next_snapshot = self.snapshot(epoch=2)
        before = self.state(owner)
        with patch.object(publication, "compile_view", side_effect=ValueError("TOP-SECRET")):
            try:
                owner.prepare(next_snapshot.revision, next_snapshot.payload)
            except receiver.RevisionError as error:
                self.assertIsNone(error.__context__)
                self.assertNotIn("TOP-SECRET", "".join(traceback.format_exception(error)))
            else:
                self.fail("invalid compilation accepted")
        self.assertEqual(self.state(owner), before)
        self.install(owner, next_snapshot)

    def test_real_validation_rejects_payload_metadata_disagreement(self):
        owner = self.owner()
        self.install(owner, self.snapshot())
        candidate = self.snapshot(epoch=2)
        value = json.loads(candidate.payload)
        value["tlsBindingHostSelectors"] = ["foreign.example.com"]
        payload = json.dumps(value, separators=(",", ":")).encode()
        revision = replace(candidate.revision, digest=hashlib.sha256(payload).hexdigest())
        before = self.state(owner)
        with self.assertRaises(receiver.RevisionError):
            owner.prepare(revision, payload)
        with self.assertRaises(receiver.RevisionError):
            owner.commit(revision)
        self.assertEqual(self.state(owner), before)

    def test_request_scope_and_policy_updates_retire_only_old_requests(self):
        for changes in ({"paths": ("/new/*",)}, {"policy_epoch": 2}):
            with self.subTest(changes=changes):
                owner = self.owner()
                first = self.snapshot()
                self.install(owner, first)
                token = self.admit(owner.registry).token
                old = owner.registry.acquire_request(token)
                second = self.snapshot(epoch=2, **changes)
                with patch.object(registry_module.time, "monotonic", return_value=10):
                    self.assertEqual(self.install(owner, second), ())
                new = owner.registry.acquire_request(token)
                self.assertEqual(new.snapshot, second)
                self.assertEqual(old.snapshot, first)
                self.assertEqual(owner.registry._request_deadlines, {old.handle.serial: 40})
                self.assertEqual(owner.registry._connection_deadlines, {})
                owner.registry.finish_request(old.handle)
                with patch.object(registry_module.time, "monotonic", return_value=40):
                    self.assertEqual(owner.registry.expired_connections(), ())

    def test_planning_failures_are_atomic_sanitized_and_retryable(self):
        class BrokenCopy(dict):
            def copy(self):
                raise MemoryError("TOP-SECRET")

        for failure in ("clock", "coverage", "copy", "result"):
            with self.subTest(failure=failure):
                owner = self.owner()
                self.install(owner, self.snapshot())
                token = self.admit(owner.registry).token
                owner.registry.acquire_request(token)
                removed = self.snapshot(epoch=2, host=None)
                owner.prepare(removed.revision, removed.payload)
                before = self.state(owner)
                if failure == "clock":
                    fault = patch.object(registry_module.time, "monotonic", side_effect=ValueError("TOP-SECRET"))
                elif failure == "coverage":
                    fault = patch.object(fixtures.selectors.Selector, "matches", side_effect=ValueError("TOP-SECRET"))
                elif failure == "copy":
                    fault = patch.object(owner.registry, "_request_deadlines", BrokenCopy())
                else:
                    fault = patch.object(registry_module, "_Activation", side_effect=MemoryError("TOP-SECRET"))
                with fault:
                    try:
                        owner.commit(removed.revision)
                    except registry_module.RegistryError as error:
                        self.assertIsNone(error.__context__)
                        self.assertNotIn("TOP-SECRET", "".join(traceback.format_exception(error)))
                    else:
                        self.fail("publication succeeded after planning failure")
                self.assertEqual(self.state(owner), before)
                self.assertEqual(owner.commit(removed.revision), (token,))

    def test_owned_pair_cannot_be_mutated_independently(self):
        owner = self.owner()
        first = self.snapshot()
        self.install(owner, first)
        before = self.state(owner)
        operations = (
            lambda: owner.registry.activate(self.snapshot(epoch=2)),
            owner.registry.deactivate,
            lambda: owner._receiver.prepare(first.revision, first.payload),
            lambda: owner._receiver.commit(first.revision),
            lambda: owner._receiver.abort(self.snapshot(epoch=2).revision),
            owner._receiver.close,
        )
        for operation in operations:
            with self.assertRaises((receiver.RevisionError, registry_module.RegistryError)):
                operation()
            self.assertEqual(self.state(owner), before)

    def test_remove_readd_retains_fence_and_first_deadline(self):
        owner = self.owner()
        self.install(owner, self.snapshot(host="*.example.com"))
        api = self.admit(owner.registry).token
        docs = self.admit(owner.registry, sni="docs.example.com").token
        with patch.object(registry_module.time, "monotonic", return_value=10):
            self.assertEqual(self.install(owner, self.snapshot(epoch=2)), (docs,))
        with patch.object(registry_module.time, "monotonic", return_value=20):
            self.install(owner, self.snapshot(epoch=3, host="*.example.com"))
            self.assertEqual(self.install(owner, self.snapshot(epoch=4)), (docs,))
        self.assertEqual(owner.registry.acquire_request(docs).reason, "connection_fenced")
        self.assertEqual(owner.registry.acquire_request(api).action, "allow")
        self.assertEqual(owner.registry._connection_deadlines, {docs.serial: 40})

    def test_close_fences_generation_and_retains_handles_deadlines_and_cleanup(self):
        owner = self.owner()
        first = self.snapshot(secret="old-secret")
        self.install(owner, first)
        token = self.admit(owner.registry).token
        request = owner.registry.acquire_request(token)
        with patch.object(registry_module.time, "monotonic", return_value=10):
            self.install(owner, self.snapshot(epoch=2))
        pending = self.snapshot(epoch=3)
        owner.prepare(pending.revision, pending.payload)
        self.assertEqual(owner.close(), (token,))
        self.assertEqual(owner.close(), (token,))
        self.assertEqual(owner.registry.acquire_request(token).reason, "registry_closed")
        self.assertEqual(self.admit(owner.registry).reason, "snapshot_missing")
        self.assertEqual(request.snapshot, first)
        self.assertIsNone(owner._pending_view)
        for operation in (
            owner.readback, owner.acquire, lambda: owner.commit(pending.revision),
            lambda: owner.prepare(pending.revision, pending.payload),
            lambda: owner.abort(pending.revision),
        ):
            with self.assertRaises(receiver.RevisionError):
                operation()
        with patch.object(registry_module.time, "monotonic", return_value=40):
            self.assertEqual(owner.registry.expired_connections(), (token,))
        metadata = repr(owner.registry.pending_requests())
        self.assertNotIn("old-secret", metadata)
        self.assertTrue(owner.registry.finish_request(request.handle))
        self.assertTrue(owner.registry.release(token))
        self.assertEqual(owner.close(), ())
        self.assertEqual(owner.registry.request_count, 0)


if __name__ == "__main__":
    unittest.main()
