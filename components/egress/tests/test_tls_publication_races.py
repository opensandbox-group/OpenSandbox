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

"""Deterministic schedules for sidecar-only joint revision publication."""

import threading
import unittest
from concurrent.futures import ThreadPoolExecutor
from functools import partial
from unittest.mock import patch

import test_tls_registry as fixtures

publication = fixtures.load("revision_publication")
receiver = fixtures.receiver
registry_module = fixtures.registry_module


class RevisionPublicationRaceTest(unittest.TestCase):
    identity = fixtures.TLSRegistryTest.identity
    snapshot = fixtures.TLSRegistryTest.snapshot
    admit = fixtures.TLSRegistryTest.admit
    assert_consistent = fixtures.TLSRequestLifecycleTest.assert_consistent

    def setUp(self):
        clock_patch = patch.object(registry_module.time, "monotonic", return_value=100.0)
        self.clock = clock_patch.start()
        self.addCleanup(clock_patch.stop)

    def setup_publisher(self, snapshot=None):
        owner = publication.RevisionPublisher(
            *self.identity, capacity=4, request_capacity=4, max_snapshot_bytes=65536,
        )
        snapshot = snapshot or self.snapshot(secret="old-private-value")
        self.assertEqual(owner.prepare(snapshot.revision, snapshot.payload), snapshot.revision)
        self.assertEqual(owner.commit(snapshot.revision), ())
        return owner, owner.acquire()

    def assert_coherent(self, owner):
        registry, active = owner.registry, owner._receiver
        with registry._lock, active._lock:
            self.assertEqual(active._closed, registry._closed)
            if active._active is None:
                self.assertIsNone(registry._view)
            else:
                self.assertEqual(active._active.revision, registry._view.revision)
            if active._pending is None:
                self.assertIsNone(owner._pending_view)
            else:
                self.assertEqual(active._pending.revision, owner._pending_view.revision)
            self.assertLessEqual(set(registry._request_deadlines), set(registry._requests))
            self.assertLessEqual(set(registry._connection_deadlines), set(registry._entries))
        self.assert_consistent(registry)

    def run_ordered(self, registry, first, second, *, at_boundary=None):
        """Make the second operation contend before the first releases its lock."""
        lock = fixtures.ObservedLock()
        registry._lock = lock
        lock.pause_next_exit = True
        with ThreadPoolExecutor(max_workers=2) as pool:
            leading = pool.submit(first)
            try:
                self.assertTrue(lock.before_unlock.wait(5))
                lock.attempted.clear()
                following = pool.submit(second)
                self.assertTrue(lock.attempted.wait(5))
                self.assertFalse(following.done())
                if at_boundary is not None:
                    at_boundary()
            finally:
                lock.resume.set()
            return leading.result(timeout=5), following.result(timeout=5)

    def test_commit_and_request_admission_have_both_orderings(self):
        for admission_first in (True, False):
            for remove_host in (False, True):
                with self.subTest(admission_first=admission_first, remove_host=remove_host):
                    owner, old = self.setup_publisher()
                    registry = owner.registry
                    token = self.admit(registry).token
                    target = self.snapshot(
                        epoch=2, host=None if remove_host else "api.example.com",
                        secret="" if remove_host else "new-private-value",
                    )
                    owner.prepare(target.revision, target.payload)
                    admit = partial(registry.acquire_request, token)
                    commit = partial(owner.commit, target.revision)
                    first, second = self.run_ordered(
                        registry, admit if admission_first else commit,
                        commit if admission_first else admit,
                    )
                    request, uncovered = (first, second) if admission_first else (second, first)
                    self.assertEqual(uncovered, (token,) if remove_host else ())
                    self.assertNotEqual(request.reason, "snapshot_mismatch")
                    if remove_host and not admission_first:
                        self.assertEqual((request.action, request.reason, request.handle),
                                         ("deny", "connection_fenced", None))
                        self.assertIsNone(request.snapshot)
                        self.assertEqual(registry.request_count, 0)
                    else:
                        self.assertEqual((request.action, request.reason), ("allow", "admitted"))
                        expected = old if admission_first else owner.acquire()
                        self.assertIs(request.snapshot, expected)
                        self.assertIs(request.handle.snapshot, expected)
                        self.assertEqual(
                            registry._request_deadlines,
                            {request.handle.serial: 130.0} if admission_first else {},
                        )
                        self.assertTrue(registry.finish_request(request.handle))
                        self.assertEqual(request.snapshot.payload, expected.payload)
                    self.assertEqual(owner.readback(), target.revision)
                    self.assertEqual(
                        registry._connection_deadlines, {token.serial: 130.0} if remove_host else {},
                    )
                    self.assert_coherent(owner)

    def test_commit_waits_for_request_before_and_after_snapshot_pin(self):
        for pinned in (False, True):
            with self.subTest(pinned=pinned):
                owner, old = self.setup_publisher()
                registry = owner.registry
                token = self.admit(registry).token
                target = self.snapshot(epoch=2, secret="new-private-value")
                owner.prepare(target.revision, target.payload)
                entered, resume = threading.Event(), threading.Event()
                lock = fixtures.ObservedLock()
                registry._lock = lock
                acquire = owner._receiver.acquire

                def paused_acquire(acquire=acquire, pinned=pinned, entered=entered, resume=resume):
                    snapshot = acquire() if pinned else None
                    entered.set()
                    if not resume.wait(5):
                        raise AssertionError("request pin timed out")
                    return snapshot if pinned else acquire()

                with (
                    patch.object(owner._receiver, "acquire", paused_acquire),
                    ThreadPoolExecutor(max_workers=2) as pool,
                ):
                    admission = pool.submit(registry.acquire_request, token)
                    try:
                        self.assertTrue(entered.wait(5))
                        self.assertTrue(lock.locked())
                        lock.attempted.clear()
                        commit = pool.submit(owner.commit, target.revision)
                        self.assertTrue(lock.attempted.wait(5))
                        self.assertFalse(commit.done())
                        self.assertEqual(registry._view.revision, old.revision)
                        self.assertIs(owner._receiver._active, old)
                    finally:
                        resume.set()
                    request = admission.result(timeout=5)
                    self.assertEqual(commit.result(timeout=5), ())
                self.assertEqual((request.action, request.reason), ("allow", "admitted"))
                self.assertIs(request.snapshot, old)
                self.assertEqual(registry._request_deadlines, {request.handle.serial: 130.0})
                self.assertTrue(registry.finish_request(request.handle))
                self.assert_coherent(owner)

    def test_half_publication_is_invisible_to_admissions_and_readback(self):
        owner, old = self.setup_publisher()
        registry = owner.registry
        token = self.admit(registry).token
        target = self.snapshot(epoch=2, secret="new-private-value")
        owner.prepare(target.revision, target.payload)
        registry_lock, receiver_lock = fixtures.ObservedLock(), fixtures.ObservedLock()
        registry._lock, owner._receiver._lock = registry_lock, receiver_lock
        entered, resume = threading.Event(), threading.Event()
        publish = registry._publish_activation

        def paused_publish(activation):
            publish(activation)
            entered.set()
            if not resume.wait(5):
                raise AssertionError("joint publication timed out")

        with (
            patch.object(registry, "_publish_activation", paused_publish),
            ThreadPoolExecutor(max_workers=4) as pool,
        ):
            commit = pool.submit(owner.commit, target.revision)
            try:
                self.assertTrue(entered.wait(5))
                self.assertEqual(registry._view.revision, target.revision)
                self.assertIs(owner._receiver._active, old)
                self.assertTrue(registry_lock.locked())
                self.assertTrue(receiver_lock.locked())
                registry_lock.attempted.clear()
                request = pool.submit(registry.acquire_request, token)
                self.assertTrue(registry_lock.attempted.wait(5))
                registry_lock.attempted.clear()
                tls = pool.submit(self.admit, registry)
                self.assertTrue(registry_lock.attempted.wait(5))
                receiver_lock.attempted.clear()
                readback = pool.submit(owner.readback)
                self.assertTrue(receiver_lock.attempted.wait(5))
                self.assertFalse(request.done())
                self.assertFalse(tls.done())
                self.assertFalse(readback.done())
            finally:
                resume.set()
            self.assertEqual(commit.result(timeout=5), ())
            admitted = request.result(timeout=5)
            connection = tls.result(timeout=5)
            self.assertEqual(readback.result(timeout=5), target.revision)
        self.assertEqual((admitted.action, admitted.reason), ("allow", "admitted"))
        self.assertEqual(admitted.snapshot.revision, target.revision)
        self.assertEqual(connection.action, "decrypt")
        self.assertEqual(connection.token.revision, target.revision)
        self.assertEqual(registry._request_deadlines, {})
        self.assertTrue(registry.finish_request(admitted.handle))
        self.assert_coherent(owner)

    def test_commit_and_tls_admission_have_both_orderings(self):
        for admission_first in (True, False):
            for remove_host in (False, True):
                with self.subTest(admission_first=admission_first, remove_host=remove_host):
                    owner, old = self.setup_publisher()
                    registry = owner.registry
                    target = self.snapshot(epoch=2, host=None if remove_host else "api.example.com")
                    owner.prepare(target.revision, target.payload)
                    admit = partial(self.admit, registry)
                    commit = partial(owner.commit, target.revision)
                    first, second = self.run_ordered(
                        registry, admit if admission_first else commit,
                        commit if admission_first else admit,
                    )
                    admission, uncovered = (first, second) if admission_first else (second, first)
                    if remove_host and not admission_first:
                        self.assertEqual((admission.action, admission.token), ("passthrough", None))
                        self.assertEqual((uncovered, registry.count), ((), 0))
                    else:
                        self.assertEqual(admission.action, "decrypt")
                        expected = old.revision if admission_first else target.revision
                        self.assertEqual(admission.token.revision, expected)
                        self.assertEqual(uncovered, (admission.token,) if remove_host else ())
                        self.assertEqual(
                            registry._request_fenced, {admission.token.serial} if remove_host else set(),
                        )
                        self.assertTrue(registry.release(admission.token))
                    self.assertEqual(owner.readback(), target.revision)
                    self.assert_coherent(owner)

    def test_commit_and_terminal_cleanup_have_both_orderings(self):
        for cleanup_first in (True, False):
            for operation in ("finish_request", "release"):
                with self.subTest(cleanup_first=cleanup_first, operation=operation):
                    owner, old = self.setup_publisher()
                    registry = owner.registry
                    token = self.admit(registry).token
                    request = registry.acquire_request(token)
                    target = self.snapshot(epoch=2, host=None)
                    owner.prepare(target.revision, target.payload)
                    argument = request.handle if operation == "finish_request" else token
                    cleanup = partial(getattr(registry, operation), argument)
                    commit = partial(owner.commit, target.revision)

                    def check_boundary(cleanup_first=cleanup_first, request=request, registry=registry):
                        expected = {} if cleanup_first else {request.handle.serial: 130.0}
                        self.assertEqual(registry._request_deadlines, expected)

                    first, second = self.run_ordered(
                        registry, cleanup if cleanup_first else commit,
                        commit if cleanup_first else cleanup, at_boundary=check_boundary,
                    )
                    cleaned, uncovered = (first, second) if cleanup_first else (second, first)
                    self.assertTrue(cleaned)
                    self.assertEqual(
                        uncovered, () if cleanup_first and operation == "release" else (token,),
                    )
                    self.assertEqual(registry.request_count, 0)
                    self.assertEqual(registry._request_deadlines, {})
                    self.assertFalse(registry.finish_request(request.handle))
                    self.assertIs(request.snapshot, old)
                    if operation == "release":
                        self.assertEqual(registry.count, 0)
                        self.assertEqual(registry._connection_deadlines, {})
                        self.assertEqual(registry._request_fenced, set())
                    else:
                        self.assertEqual(registry.count, 1)
                        self.assertEqual(registry._connection_deadlines, {token.serial: 130.0})
                    self.assertEqual(owner.readback(), target.revision)
                    self.assert_coherent(owner)

    def test_commit_and_close_have_both_orderings(self):
        for commit_first in (True, False):
            with self.subTest(commit_first=commit_first):
                owner, old = self.setup_publisher()
                registry = owner.registry
                token = self.admit(registry).token
                request = registry.acquire_request(token)
                target = self.snapshot(epoch=2, host=None)
                owner.prepare(target.revision, target.payload)
                commit = partial(owner.commit, target.revision)
                if commit_first:
                    self.assertEqual(
                        self.run_ordered(registry, commit, owner.close), ((token,), (token,)),
                    )
                else:
                    with self.assertRaisesRegex(receiver.RevisionError, "receiver closed"):
                        self.run_ordered(registry, owner.close, commit)
                self.assertEqual(owner.close(), (token,))
                self.assertEqual(registry.acquire_request(token).reason, "registry_closed")
                self.assertEqual(self.admit(registry).reason, "snapshot_missing")
                self.assertIs(request.snapshot, old)
                self.assertEqual(
                    registry._request_deadlines,
                    {request.handle.serial: 130.0} if commit_first else {},
                )
                self.assertEqual(
                    registry._connection_deadlines, {token.serial: 130.0} if commit_first else {},
                )
                self.assert_coherent(owner)
                self.assertTrue(registry.finish_request(request.handle))
                self.assertTrue(registry.release(token))
                self.assert_coherent(owner)

    def test_abort_and_close_finish_while_compile_is_blocked_and_prevent_staging(self):
        for operation in ("abort", "close"):
            with self.subTest(operation=operation):
                owner, old = self.setup_publisher()
                target = self.snapshot(epoch=2)
                entered, resume = threading.Event(), threading.Event()
                compile_view = publication.compile_view

                def paused_compile(snapshot, entered=entered, resume=resume, compile_view=compile_view):
                    entered.set()
                    if not resume.wait(5):
                        raise AssertionError("selector compilation timed out")
                    return compile_view(snapshot)

                with (
                    patch.object(publication, "compile_view", paused_compile),
                    ThreadPoolExecutor(max_workers=2) as pool,
                ):
                    prepare = pool.submit(owner.prepare, target.revision, target.payload)
                    try:
                        self.assertTrue(entered.wait(5))
                        if operation == "abort":
                            terminal = pool.submit(owner.abort, target.revision)
                            self.assertEqual(terminal.result(timeout=5), target.revision)
                            self.assertIs(owner.acquire(), old)
                        else:
                            terminal = pool.submit(owner.close)
                            self.assertEqual(terminal.result(timeout=5), ())
                        self.assertFalse(prepare.done())
                        self.assertIsNone(owner._receiver._pending)
                        self.assertIsNone(owner._pending_view)
                    finally:
                        resume.set()
                    with self.assertRaises(receiver.RevisionError):
                        prepare.result(timeout=5)
                self.assertIsNone(owner._receiver._pending)
                self.assertIsNone(owner._pending_view)
                with self.assertRaises(receiver.RevisionError):
                    owner.commit(target.revision)
                with self.assertRaises(receiver.RevisionError):
                    owner.prepare(target.revision, target.payload)
                self.assert_coherent(owner)

    def test_concurrent_prepares_deduplicate_exact_bytes_or_preserve_winner(self):
        for same_candidate in (True, False):
            for winner in (0, 1):
                with self.subTest(same_candidate=same_candidate, winner=winner):
                    owner, old = self.setup_publisher()
                    target = self.snapshot(epoch=2, secret="candidate-two")
                    other = target if same_candidate else self.snapshot(epoch=3, host="other.example.com")
                    candidates = (target, other)
                    entered = [threading.Event(), threading.Event()]
                    resume = [threading.Event(), threading.Event()]
                    compiler_lock = threading.Lock()
                    compiled = []
                    compile_view = publication.compile_view

                    def paused_compile(
                        snapshot, compile_view=compile_view, compiler_lock=compiler_lock,
                        compiled=compiled, entered=entered, resume=resume,
                    ):
                        view = compile_view(snapshot)
                        with compiler_lock:
                            slot = len(compiled)
                            compiled.append(view)
                        entered[slot].set()
                        if not resume[slot].wait(5):
                            raise AssertionError("concurrent preparation timed out")
                        return view

                    with (
                        patch.object(publication, "compile_view", paused_compile),
                        ThreadPoolExecutor(max_workers=2) as pool,
                    ):
                        futures = [pool.submit(owner.prepare, target.revision, target.payload)]
                        try:
                            self.assertTrue(entered[0].wait(5))
                            futures.append(pool.submit(owner.prepare, other.revision, other.payload))
                            self.assertTrue(entered[1].wait(5))
                            self.assertIsNone(owner._receiver._pending)
                            resume[winner].set()
                            self.assertEqual(futures[winner].result(timeout=5), candidates[winner].revision)
                            pending = owner._receiver._pending
                            self.assertIs(owner._pending_view, compiled[winner])
                            resume[1 - winner].set()
                            if same_candidate:
                                self.assertEqual(futures[1 - winner].result(timeout=5), target.revision)
                            else:
                                with self.assertRaisesRegex(receiver.RevisionError, "another revision is prepared"):
                                    futures[1 - winner].result(timeout=5)
                            self.assertIs(owner._receiver._pending, pending)
                            self.assertIs(owner._pending_view, compiled[winner])
                        finally:
                            for event in resume:
                                event.set()
                    self.assertIs(owner.acquire(), old)
                    self.assertEqual(owner._receiver._highest_epoch, candidates[winner].revision.decision_epoch)
                    self.assert_coherent(owner)
                    self.assertEqual(owner.commit(candidates[winner].revision), ())
                    self.assertEqual(owner.readback(), candidates[winner].revision)
                    self.assert_coherent(owner)

    def test_delayed_active_exact_retry_preserves_newer_pending_candidate(self):
        owner, old = self.setup_publisher(self.snapshot(host="*.example.com", secret="old-private-value"))
        registry = owner.registry
        token = self.admit(registry, sni="docs.example.com").token
        request = registry.acquire_request(token)
        target = self.snapshot(epoch=2, host="api.example.com")
        newer = self.snapshot(epoch=3, host="*.example.com")
        entered, resume = threading.Event(), threading.Event()
        compile_view = publication.compile_view
        first_compile = True
        compiler_lock = threading.Lock()

        def delayed_compile(snapshot):
            nonlocal first_compile
            with compiler_lock:
                delay = first_compile
                first_compile = False
            if delay:
                entered.set()
                if not resume.wait(5):
                    raise AssertionError("delayed exact retry timed out")
            return compile_view(snapshot)

        with (
            patch.object(publication, "compile_view", delayed_compile),
            ThreadPoolExecutor(max_workers=1) as pool,
        ):
            delayed = pool.submit(owner.prepare, target.revision, target.payload)
            try:
                self.assertTrue(entered.wait(5))
                owner.prepare(target.revision, target.payload)
                self.assertEqual(owner.commit(target.revision), (token,))
                self.clock.return_value = 110.0
                owner.prepare(newer.revision, newer.payload)
                pending, pending_view = owner._receiver._pending, owner._pending_view
            finally:
                resume.set()
            self.assertEqual(delayed.result(timeout=5), target.revision)
        self.assertIs(owner._receiver._pending, pending)
        self.assertIs(owner._pending_view, pending_view)
        self.assertEqual(owner.commit(target.revision), ())
        self.assertIs(owner._receiver._pending, pending)
        self.assertIs(owner._pending_view, pending_view)
        self.assertEqual(registry._connection_deadlines, {token.serial: 130.0})
        self.assertEqual(registry._request_deadlines, {request.handle.serial: 130.0})
        self.assertIs(request.snapshot, old)
        self.assert_coherent(owner)
        self.assertEqual(owner.commit(newer.revision), ())
        self.assertEqual(owner.readback(), newer.revision)
        self.assertEqual(registry._connection_deadlines, {token.serial: 130.0})
        self.assertEqual(registry._request_deadlines, {request.handle.serial: 130.0})
        self.assertEqual(registry.acquire_request(token).reason, "connection_fenced")
        self.assert_coherent(owner)


if __name__ == "__main__":
    unittest.main()
