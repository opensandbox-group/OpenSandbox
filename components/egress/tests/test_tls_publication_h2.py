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

"""Pure H2 admission/publication tests; no sockets or transport completion claim."""

import base64
import io
import json
import threading
import traceback
import unittest
from concurrent.futures import ThreadPoolExecutor
from email.message import Message
from functools import partial
from types import SimpleNamespace
from unittest.mock import patch

import test_tls_publication as publication_tests
import test_tls_publication_races as races
import test_tls_registry as fixtures

publication = fixtures.load("revision_publication")
ipc = fixtures.load("revision_ipc")
registry_module = fixtures.registry_module


class H2RevisionPublicationTest(unittest.TestCase):
    identity = fixtures.TLSRegistryTest.identity
    snapshot = fixtures.TLSRegistryTest.snapshot
    admit = fixtures.TLSRegistryTest.admit
    run_ordered = races.RevisionPublicationRaceTest.run_ordered
    state = publication_tests.RevisionPublicationTest.state

    def setUp(self):
        clock = patch.object(registry_module.time, "monotonic", return_value=100.0)
        self.clock = clock.start()
        self.addCleanup(clock.stop)
        self.backend = publication.LiveReceiver(
            *self.identity, max_snapshot_bytes=65536, capacity=4, request_capacity=4,
        )
        self.registry = self.backend.registry
        self.first = self.snapshot(secret="old-private-value")
        self.install(self.first)
        self.token = self.admit(self.registry).token

    def install(self, snapshot):
        self.assertEqual(self.backend.prepare(snapshot.revision, snapshot.payload), snapshot.revision)
        self.assertEqual(self.backend.commit(snapshot.revision), snapshot.revision)

    def command(self, operation, snapshot=None, *, at_reply=None):
        # The actual authenticated parser and dispatch are used. Only its network
        # response writer is replaced; this is not an AF_UNIX or runtime test.
        handler = ipc._Handler.__new__(ipc._Handler)
        value = {"revision": ipc._to_wire(snapshot.revision)} if snapshot else {}
        if operation == "prepare":
            value["payload"] = base64.b64encode(snapshot.payload).decode()
        body = json.dumps(value).encode()
        handler.headers = Message()
        handler.headers["Authorization"] = "Bearer " + "a" * 32
        handler.headers["Content-Type"] = "application/json"
        handler.headers["Content-Length"] = str(len(body))
        handler.rfile = io.BytesIO(body)
        handler.server = SimpleNamespace(
            receiver=self.backend, session_token="a" * 32,
            max_snapshot_bytes=65536, max_request_bytes=100000,
        )
        handler.path = "/v1/revisions/" + operation
        replies = []

        def reply(status, value):
            if at_reply is not None:
                at_reply()
            replies.append((status, value))

        handler._reply = reply
        handler.do_POST()
        self.assertEqual(len(replies), 1)
        return replies[0]

    def test_real_live_receiver_has_fence_and_frozen_bound_before_parser_ack(self):
        request = self.registry.acquire_request(self.token, client_stream_id=17)
        removed = self.snapshot(epoch=2, host=None)
        self.assertIs(type(self.backend), publication.LiveReceiver)
        expected = (200, {"revision": ipc._to_wire(removed.revision)})
        self.assertEqual(self.command("prepare", removed), expected)

        def before_ack():
            self.assertEqual(self.backend.readback(), removed.revision)
            self.assertEqual(self.registry.drain_state(self.token),
                             registry_module.DrainState(True, 17, 130.0, 17))
            self.assertEqual(self.registry.acquire_request(self.token, client_stream_id=19).reason,
                             "connection_fenced")
            self.assertIs(request.snapshot, request.handle.snapshot)
            self.assertEqual(request.snapshot, self.first)

        self.assertEqual(self.command("commit", removed, at_reply=before_ack), expected)
        self.clock.return_value = 120.0
        self.assertEqual(self.command("commit", removed, at_reply=before_ack), expected)

    def test_first_h2_attachment_after_removal_observes_zero_bound(self):
        self.install(self.snapshot(epoch=2, host=None))
        self.assertEqual(self.registry.drain_state(self.token),
                         registry_module.DrainState(True, 0, 130.0, 0))
        self.assertEqual(self.registry.acquire_request(self.token, client_stream_id=1).reason,
                         "connection_fenced")
        self.clock.return_value = 120.0
        self.install(self.snapshot(epoch=3))
        self.install(self.snapshot(epoch=4, host=None))
        self.assertEqual(self.registry.drain_state(self.token),
                         registry_module.DrainState(True, 0, 130.0, 0))
        self.assertEqual(self.registry.request_count, 0)

    def test_admission_and_commit_have_both_atomic_orderings(self):
        for admission_first in (True, False):
            for remove_host in (True, False):
                with self.subTest(admission_first=admission_first, remove_host=remove_host):
                    backend = publication.LiveReceiver(
                        *self.identity, max_snapshot_bytes=65536, capacity=1,
                    )
                    backend.prepare(self.first.revision, self.first.payload)
                    backend.commit(self.first.revision)
                    registry = backend.registry
                    token = self.admit(registry).token
                    target = self.snapshot(
                        epoch=2, host=None if remove_host else "api.example.com",
                        secret="" if remove_host else "new-private-value",
                    )
                    backend.prepare(target.revision, target.payload)
                    admit = partial(registry.acquire_request, token, client_stream_id=29)
                    commit = partial(backend.commit, target.revision)
                    leading, following = self.run_ordered(
                        registry, admit if admission_first else commit,
                        commit if admission_first else admit,
                    )
                    request, acknowledged = ((leading, following) if admission_first
                                             else (following, leading))
                    self.assertEqual(acknowledged, target.revision)
                    state = registry.drain_state(token)
                    if remove_host and not admission_first:
                        self.assertEqual(request.reason, "connection_fenced")
                        self.assertIsNone(request.handle)
                        self.assertIsNone(request.snapshot)
                        self.assertEqual(state, registry_module.DrainState(True, 0, 130.0, 0))
                    else:
                        self.assertEqual(request.action, "allow")
                        self.assertEqual(request.snapshot, self.first if admission_first else target)
                        self.assertIs(request.handle.snapshot, request.snapshot)
                        self.assertEqual(request.handle.client_stream_id, 29)
                        self.assertEqual(state, registry_module.DrainState(
                            remove_host, 29 if remove_host else None,
                            130.0 if admission_first else None, 29,
                        ))
                    self.assertEqual(backend.readback(), target.revision)

    def test_commit_cannot_split_snapshot_pin_from_stream_registration(self):
        for pinned in (False, True):
            with self.subTest(pinned=pinned):
                backend = publication.LiveReceiver(
                    *self.identity, max_snapshot_bytes=65536, capacity=1,
                )
                backend.prepare(self.first.revision, self.first.payload)
                backend.commit(self.first.revision)
                registry = backend.registry
                token = self.admit(registry).token
                target = self.snapshot(epoch=2, host=None)
                backend.prepare(target.revision, target.payload)
                active = backend._publisher._receiver
                acquire = active.acquire
                entered, resume = threading.Event(), threading.Event()
                lock = fixtures.ObservedLock()
                registry._lock = lock

                def paused_acquire():
                    snapshot = acquire() if pinned else None
                    entered.set()
                    if not resume.wait(5):
                        raise AssertionError("request pin timed out")
                    return snapshot if pinned else acquire()

                with patch.object(active, "acquire", paused_acquire):
                    with ThreadPoolExecutor(max_workers=2) as pool:
                        admission = pool.submit(registry.acquire_request, token, client_stream_id=31)
                        try:
                            self.assertTrue(entered.wait(5))
                            self.assertTrue(lock.locked())
                            lock.attempted.clear()
                            commit = pool.submit(backend.commit, target.revision)
                            self.assertTrue(lock.attempted.wait(5))
                            self.assertFalse(commit.done())
                            self.assertEqual(registry._max_admitted_stream_ids, {})
                        finally:
                            resume.set()
                        request = admission.result(5)
                        self.assertEqual(commit.result(5), target.revision)
                self.assertEqual(request.snapshot, self.first)
                self.assertEqual(request.handle.client_stream_id, 31)
                self.assertEqual(registry.drain_state(token),
                                 registry_module.DrainState(True, 31, 130.0, 31))

    def test_drain_reader_cannot_observe_half_joint_publication(self):
        self.registry.acquire_request(self.token, client_stream_id=5)
        target = self.snapshot(epoch=2, host=None)
        self.backend.prepare(target.revision, target.payload)
        registry_lock, receiver_lock = fixtures.ObservedLock(), fixtures.ObservedLock()
        self.registry._lock = registry_lock
        self.backend._publisher._receiver._lock = receiver_lock
        entered, resume = threading.Event(), threading.Event()
        publish = self.registry._publish_activation

        def paused_publish(activation):
            publish(activation)
            entered.set()
            if not resume.wait(5):
                raise AssertionError("publication timed out")

        with patch.object(self.registry, "_publish_activation", paused_publish):
            with ThreadPoolExecutor(max_workers=3) as pool:
                commit = pool.submit(self.backend.commit, target.revision)
                try:
                    self.assertTrue(entered.wait(5))
                    self.assertEqual(self.registry._frozen_last_stream_ids, {self.token.serial: 5})
                    self.assertEqual(self.backend._publisher._receiver._active, self.first)
                    registry_lock.attempted.clear()
                    drain = pool.submit(self.registry.drain_state, self.token)
                    self.assertTrue(registry_lock.attempted.wait(5))
                    receiver_lock.attempted.clear()
                    readback = pool.submit(self.backend.readback)
                    self.assertTrue(receiver_lock.attempted.wait(5))
                    self.assertFalse(drain.done())
                    self.assertFalse(readback.done())
                finally:
                    resume.set()
                self.assertEqual(commit.result(5), target.revision)
                self.assertEqual(readback.result(5), target.revision)
                self.assertEqual(drain.result(5), registry_module.DrainState(True, 5, 130.0, 5))

    def test_rotation_deadline_survives_retained_handle_until_transport_completion(self):
        for changed in ({"secret": "rotated-private-value"}, {"paths": ("/new/*",)},
                        {"policy_epoch": 2}):
            with self.subTest(changed=changed):
                backend = publication.LiveReceiver(
                    *self.identity, max_snapshot_bytes=65536, capacity=1, request_capacity=3,
                )
                backend.prepare(self.first.revision, self.first.payload)
                backend.commit(self.first.revision)
                registry = backend.registry
                token = self.admit(registry).token
                old = registry.acquire_request(token, client_stream_id=3).handle
                target = self.snapshot(epoch=2, **changed)
                self.clock.return_value = 100.0
                backend.prepare(target.revision, target.payload)
                backend.commit(target.revision)
                new = registry.acquire_request(token, client_stream_id=5).handle
                # Application completion alone does not call finish_request.
                # This retained old handle represents outstanding H2 output.
                self.assertEqual(registry.drain_state(token),
                                 registry_module.DrainState(False, None, 130.0, 5))
                self.assertEqual(old.snapshot, self.first)
                self.assertEqual(new.snapshot, target)
                self.clock.return_value = 110.0
                later = self.snapshot(epoch=3)
                backend.prepare(later.revision, later.payload)
                backend.commit(later.revision)
                self.assertEqual(registry.drain_state(token).deadline, 130.0)
                self.assertEqual(registry._request_deadlines, {old.serial: 130.0, new.serial: 140.0})
                self.clock.return_value = 130.0
                self.assertEqual(registry.expired_connections(), (token,))
                self.assertTrue(registry.finish_request(old))
                self.assertEqual(registry.drain_state(token).deadline, 140.0)
                self.assertEqual(registry.expired_connections(), ())
                self.assertTrue(registry.finish_request(new))
                self.assertIsNone(registry.drain_state(token).deadline)

    def test_minimum_deadline_and_first_frozen_bound_survive_remove_readd(self):
        high = self.registry.acquire_request(self.token, client_stream_id=19).handle
        low = self.registry.acquire_request(self.token, client_stream_id=7).handle
        self.assertTrue(self.registry.finish_request(high))
        second = self.snapshot(epoch=2, secret="new-private-value")
        self.install(second)
        new = self.registry.acquire_request(self.token, client_stream_id=21).handle
        self.clock.return_value = 110.0
        removed = self.snapshot(epoch=3, host=None)
        self.install(removed)
        expected = registry_module.DrainState(True, 21, 130.0, 21)
        self.assertEqual(self.registry.drain_state(self.token), expected)
        self.assertEqual(self.registry._connection_deadlines, {self.token.serial: 140.0})
        self.clock.return_value = 120.0
        self.assertEqual(self.backend.commit(removed.revision), removed.revision)
        self.install(self.snapshot(epoch=4))
        self.install(self.snapshot(epoch=5, host=None))
        self.assertEqual(self.registry.drain_state(self.token), expected)
        self.assertTrue(self.registry.finish_request(low))
        self.assertEqual(self.registry.drain_state(self.token).deadline, 140.0)
        self.assertTrue(self.registry.finish_request(new))
        self.assertEqual(self.registry.drain_state(self.token),
                         registry_module.DrainState(True, 21, 140.0, 21))

    def test_frozen_plan_allocation_failure_is_atomic_sanitized_and_retryable(self):
        class BrokenCopy(dict):
            def copy(self):
                raise MemoryError("private-plan-value")

        class BrokenSetDefault(dict):
            def setdefault(self, key, value):
                super().setdefault(key, value)
                raise MemoryError("private-plan-value")

        class BrokenPreparedCopy(dict):
            def copy(self):
                return BrokenSetDefault(self)

        self.registry.acquire_request(self.token, client_stream_id=23)
        removed = self.snapshot(epoch=2, host=None)
        self.backend.prepare(removed.revision, removed.payload)
        owner = self.backend._publisher
        before = self.state(owner)
        for failure in (BrokenCopy, BrokenPreparedCopy):
            with self.subTest(failure=failure.__name__):
                with patch.object(self.registry, "_frozen_last_stream_ids", failure()):
                    try:
                        self.backend.commit(removed.revision)
                    except registry_module.RegistryError as error:
                        self.assertEqual(str(error), "joint revision publication failed")
                        self.assertIsNone(error.__context__)
                        self.assertNotIn("private-plan-value", "".join(traceback.format_exception(error)))
                    else:
                        self.fail("partial publication was accepted")
                self.assertEqual(self.state(owner), before)
                self.assertEqual(self.registry.drain_state(self.token),
                                 registry_module.DrainState(False, None, None, 23))
        with patch.object(registry_module, "_Activation", side_effect=MemoryError("private-plan-value")):
            self.assertEqual(self.command("commit", removed), (500, {"error": "internal_error"}))
        self.assertEqual(self.state(owner), before)
        self.assertEqual(self.backend.commit(removed.revision), removed.revision)
        self.assertEqual(self.registry.drain_state(self.token),
                         registry_module.DrainState(True, 23, 130.0, 23))


if __name__ == "__main__":
    unittest.main()
