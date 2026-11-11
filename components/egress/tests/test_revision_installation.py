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

"""Installation-only IPC backend: coherent publication without admissions."""

import base64
import io
import json
import threading
import unittest
from concurrent.futures import ThreadPoolExecutor
from email.message import Message
from types import SimpleNamespace
from unittest.mock import patch

import test_tls_registry as fixtures

publication = fixtures.load("revision_publication")
ipc = fixtures.load("revision_ipc")
receiver = fixtures.receiver


class RevisionInstallationTest(unittest.TestCase):
    identity = fixtures.TLSRegistryTest.identity
    snapshot = fixtures.TLSRegistryTest.snapshot
    admit = fixtures.TLSRegistryTest.admit

    def setUp(self):
        self.backend = publication.InstallationReceiver(
            *self.identity, max_snapshot_bytes=65536,
        )

    def install(self, snapshot):
        self.assertEqual(self.backend.prepare(snapshot.revision, snapshot.payload), snapshot.revision)
        self.assertEqual(self.backend.commit(snapshot.revision), snapshot.revision)
        self.assertEqual(self.backend.acquire(), snapshot)
        self.assertEqual(self.backend._publisher.registry._view.revision, snapshot.revision)

    def test_empty_bound_rotation_and_removal_publish_both_views(self):
        for snapshot in (
            self.snapshot(host=None), self.snapshot(epoch=2, secret="private-old"),
            self.snapshot(epoch=3, secret="private-new"), self.snapshot(epoch=4, host=None),
        ):
            self.install(snapshot)
            self.assertEqual(self.backend.readback(), snapshot.revision)

    def test_admissions_are_disabled_before_install_after_install_and_close(self):
        self.assertFalse(hasattr(self.backend, "registry"))
        registry = self.backend._publisher.registry
        for phase in ("before", "installed", "closed"):
            if phase == "installed":
                self.install(self.snapshot())
            elif phase == "closed":
                self.backend.close()
            for sni, ech in (("api.example.com", False), ("unbound.example.com", False), (None, False), ("api.example.com", True)):
                decision = self.admit(registry, sni=sni, ech_hidden=ech)
                self.assertEqual((decision.action, decision.reason, decision.token), ("deny", "admission_disabled", None))
            self.assertEqual(registry.count, 0)
            self.assertEqual(registry.request_count, 0)
            self.assertEqual(registry.acquire_request(None).action, "deny")

    def test_active_retry_preserves_newer_candidate_and_abort_preserves_active(self):
        first, second = self.snapshot(), self.snapshot(epoch=2, host=None)
        self.install(first)
        self.backend.prepare(second.revision, second.payload)
        self.assertEqual(self.backend.commit(first.revision), first.revision)
        self.assertEqual(self.backend.commit(second.revision), second.revision)
        third = self.snapshot(epoch=3)
        self.backend.prepare(third.revision, third.payload)
        self.assertEqual(self.backend.abort(third.revision), third.revision)
        self.assertEqual(self.backend.readback(), second.revision)
        with self.assertRaises(receiver.RevisionError):
            self.backend.commit(third.revision)

    def test_compile_and_planning_failures_leave_previous_pair_active(self):
        first, second = self.snapshot(), self.snapshot(epoch=2, secret="private-new")
        self.install(first)
        with (
            patch.object(publication, "compile_view", side_effect=RuntimeError("private-new")),
            self.assertRaisesRegex(receiver.RevisionError, "^snapshot validation failed$"),
        ):
            self.backend.prepare(second.revision, second.payload)
        self.assertEqual(self.backend.acquire(), first)
        self.backend.prepare(second.revision, second.payload)
        with (
            patch.object(self.backend._publisher.registry, "_plan_activation", side_effect=RuntimeError("private-new")),
            self.assertRaisesRegex(fixtures.registry_module.RegistryError, "^joint revision publication failed$"),
        ):
            self.backend.commit(second.revision)
        self.assertEqual(self.backend.acquire(), first)
        self.assertEqual(self.backend._publisher.registry._view.revision, first.revision)
        self.assertEqual(self.backend.commit(second.revision), second.revision)

    def test_close_fences_both_views_and_preserves_pinned_bytes(self):
        first = self.snapshot(secret="private-old")
        self.install(first)
        pinned = self.backend.acquire()
        self.assertIsNone(self.backend.close())
        self.assertIsNone(self.backend.close())
        self.assertEqual(pinned, first)
        self.assertTrue(self.backend._publisher.registry._closed)
        self.assertIsNone(self.backend._publisher.registry._view)
        with self.assertRaises(receiver.RevisionError):
            self.backend.readback()
        with self.assertRaises(receiver.RevisionError):
            self.backend.commit(first.revision)

    def test_close_invalidates_delayed_prepare(self):
        entered, resume = threading.Event(), threading.Event()
        original = publication.compile_view
        def delayed(snapshot):
            entered.set()
            self.assertTrue(resume.wait(5))
            return original(snapshot)
        candidate = self.snapshot()
        with patch.object(publication, "compile_view", side_effect=delayed), ThreadPoolExecutor(max_workers=1) as pool:
            preparing = pool.submit(self.backend.prepare, candidate.revision, candidate.payload)
            try:
                self.assertTrue(entered.wait(5))
                self.backend.close()
            finally:
                resume.set()
            with self.assertRaises(receiver.RevisionError):
                preparing.result(timeout=5)
        self.assertIsNone(self.backend._publisher._pending_view)
        self.assertIsNone(self.backend._publisher.registry._view)

    def command(self, operation, snapshot=None, *, token="a" * 32):
        # Exercise the real HTTP command parser/dispatcher, replacing only the
        # network response writer. The Unix socket suite covers transport.
        handler = ipc._Handler.__new__(ipc._Handler)
        value = {"revision": ipc._to_wire(snapshot.revision)} if snapshot else {}
        if operation == "prepare":
            value["payload"] = base64.b64encode(snapshot.payload).decode()
        body = json.dumps(value).encode()
        handler.headers = Message()
        handler.headers["Authorization"] = "Bearer " + token
        handler.headers["Content-Type"] = "application/json"
        handler.headers["Content-Length"] = str(len(body))
        handler.rfile = io.BytesIO(body)
        handler.server = SimpleNamespace(receiver=self.backend, session_token="a" * 32, max_snapshot_bytes=65536, max_request_bytes=100000)
        handler.path = "/v1/revisions/" + operation
        replies = []
        handler._reply = lambda status, value: replies.append((status, value))
        if operation == "active":
            handler.headers.replace_header("Content-Length", "0")
            handler.do_GET()
        else:
            handler.do_POST()
        return replies[0]

    def test_wire_ack_is_exact_metadata_and_lost_ack_readback_is_coherent(self):
        first, second = self.snapshot(secret="private-old"), self.snapshot(epoch=2, host=None)
        for candidate in (first, second):
            expected = (200, {"revision": ipc._to_wire(candidate.revision)})
            self.assertEqual(self.command("prepare", candidate), expected)
            self.assertEqual(self.command("commit", candidate), expected)
            self.assertEqual(self.command("active"), expected)
            self.assertEqual(self.command("commit", candidate), expected)
        self.assertEqual(self.backend._publisher.registry._view.revision, second.revision)

    def test_unauthorized_commands_cannot_change_either_view(self):
        first, second = self.snapshot(), self.snapshot(epoch=2, host=None)
        self.install(first)
        self.backend.prepare(second.revision, second.payload)
        for operation in ("prepare", "commit", "abort", "active"):
            self.assertEqual(self.command(operation, second, token="bad"), (401, {"error": "unauthorized"}))
            self.assertEqual(self.backend.acquire(), first)
            self.assertEqual(self.backend._publisher.registry._view.revision, first.revision)
        self.assertEqual(self.backend.commit(second.revision), second.revision)

    def test_commit_ack_does_not_read_a_concurrently_installed_successor(self):
        first, second = self.snapshot(), self.snapshot(epoch=2, host=None)
        self.backend.prepare(first.revision, first.payload)
        published, resume = threading.Event(), threading.Event()
        original = self.backend._publisher.commit
        def delayed(revision):
            result = original(revision)
            if revision == first.revision:
                published.set()
                self.assertTrue(resume.wait(5))
            return result
        with patch.object(self.backend._publisher, "commit", side_effect=delayed), ThreadPoolExecutor(max_workers=1) as pool:
            committing = pool.submit(self.command, "commit", first)
            try:
                self.assertTrue(published.wait(5))
                self.backend.prepare(second.revision, second.payload)
                self.assertEqual(self.backend.commit(second.revision), second.revision)
            finally:
                resume.set()
            self.assertEqual(committing.result(timeout=5), (200, {"revision": ipc._to_wire(first.revision)}))
        self.assertEqual(self.backend.readback(), second.revision)

    def test_wire_failures_hide_secrets_and_leave_active_state(self):
        first, second = self.snapshot(), self.snapshot(epoch=2, secret="private-new")
        self.install(first)
        self.backend.prepare(second.revision, second.payload)
        with patch.object(self.backend._publisher.registry, "_plan_activation", side_effect=RuntimeError("private-new /private/path")):
            self.assertEqual(self.command("commit", second), (500, {"error": "internal_error"}))
        self.assertEqual(self.command("active"), (200, {"revision": ipc._to_wire(first.revision)}))
        alien = self.snapshot(epoch=3, identity=("other", "subject-a"))
        self.assertEqual(self.command("prepare", alien), (409, {"error": "revision_rejected"}))

    def test_live_overlap_and_compile_failures_keep_wire_rejection_contract(self):
        from static_passthrough import parse_handoff
        self.backend = publication.LiveReceiver(
            *self.identity, max_snapshot_bytes=65536, capacity=8,
            static_passthrough=parse_handoff('["api.example.com"]'),
        )
        first = self.snapshot(host="other.example.net")
        self.install(first)
        candidate = self.snapshot(epoch=2, secret="private-new")
        rejected = (409, {"error": "revision_rejected"})
        self.assertEqual(self.command("prepare", candidate), rejected)
        with patch.object(publication, "compile_view", side_effect=ValueError("private-new")):
            self.assertEqual(self.command("prepare", candidate), rejected)
        self.assertEqual(self.backend.acquire(), first)
        self.assertIsNone(self.backend._publisher._pending_view)


if __name__ == "__main__":
    unittest.main()
