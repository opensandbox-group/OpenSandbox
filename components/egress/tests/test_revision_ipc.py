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

"""Authenticated Unix IPC contract for the OSEP-0023 revision receiver."""

import base64
import hashlib
import http.client
import importlib.util
import json
import os
import select
import shutil
import socket
import stat
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest import mock

MITMSCRIPTS = Path(__file__).resolve().parents[1] / "mitmscripts"
TOKEN = "0123456789abcdef0123456789abcdef"


def load(name):
    if name in sys.modules:
        return sys.modules[name]
    spec = importlib.util.spec_from_file_location(name, MITMSCRIPTS / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[name] = module
    spec.loader.exec_module(module)
    return module


receiver = load("revision_receiver")
ipc = load("revision_ipc")


class UnixConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("revision-ipc", timeout=2)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


class LateBodyConnection(UnixConnection):
    """Send the body only after the server has replied and closed."""

    def __init__(self, path):
        super().__init__(path)
        self.sends = 0

    def send(self, data):
        self.sends += 1
        if self.sends == 2:  # http.client writes headers and body separately
            select.select([self.sock], [], [], self.timeout)
            time.sleep(0.05)
        super().send(data)


class RevisionIPCTest(unittest.TestCase):
    connection_class = UnixConnection

    def setUp(self):
        self.directory = tempfile.mkdtemp(prefix="osri-", dir="/tmp")
        self.path = os.path.join(self.directory, "receiver.sock")
        self.validated = []
        self.receiver = receiver.Receiver(
            "control-a",
            "subject-a",
            self.validated.append,
            max_snapshot_bytes=128,
        )
        self.server = None

    def tearDown(self):
        if self.server is not None:
            self.server.close()
        shutil.rmtree(self.directory)

    def start(self):
        self.server = ipc.Server(
            self.receiver,
            self.path,
            TOKEN,
            max_snapshot_bytes=128,
            request_timeout=1,
        )
        self.server.start()

    def revision(self, epoch, vault, payload):
        return {
            "controlGeneration": "control-a",
            "subjectGeneration": "subject-a",
            "decisionEpoch": epoch,
            "vaultRevision": vault,
            "policyEpoch": 3,
            "digest": hashlib.sha256(payload).hexdigest(),
        }

    def request(self, method, target, value=None, *, token=TOKEN, raw=None):
        body = raw if raw is not None else None if value is None else json.dumps(value)
        headers = {"Authorization": f"Bearer {token}"}
        if body is not None:
            headers["Content-Type"] = "application/json"
        connection = self.connection_class(self.path)
        try:
            connection.request(method, target, body=body, headers=headers)
        except (BrokenPipeError, ConnectionResetError):
            # 401/404 replies fail closed without reading the body and close the
            # socket, which can beat http.client's separate body write. The reply
            # is already complete; with no reply, getresponse() still raises.
            pass
        response = connection.getresponse()
        data = response.read()
        connection.close()
        return response.status, json.loads(data)

    def test_go_wire_transaction_and_metadata_only_readback(self):
        self.start()
        self.assertEqual(stat.S_IMODE(os.stat(self.path).st_mode), 0o600)
        payload = b""
        one = self.revision(1, 0, payload)
        prepare = {"revision": one, "payload": ""}  # Go's empty []byte encoding
        self.assertEqual(
            self.request("POST", "/v1/revisions/prepare", prepare),
            (200, {"revision": one}),
        )
        self.assertEqual(
            self.request("GET", "/v1/revisions/active"), (200, {"revision": None})
        )
        self.assertEqual(
            self.request("POST", "/v1/revisions/commit", {"revision": one}),
            (200, {"revision": one}),
        )
        self.assertEqual(
            self.request("GET", "/v1/revisions/active"), (200, {"revision": one})
        )
        self.assertEqual(self.receiver.acquire().payload, payload)

        payload = b'{"revision":1,"secret":"never-log-me"}'
        two = self.revision(2, 1, payload)
        self.assertEqual(
            self.request("POST", "/v1/revisions/abort", {"revision": two}),
            (200, {"revision": two}),
        )
        self.assertEqual(self.receiver.readback().decision_epoch, 1)
        self.assertNotIn(
            "never-log-me", json.dumps(self.request("GET", "/v1/revisions/active"))
        )

    def test_authentication_and_malformed_requests_fail_closed(self):
        self.start()
        payload = b'{"secret":"never-log-me"}'
        revision = self.revision(1, 1, payload)
        envelope = {"revision": revision, "payload": base64.b64encode(payload).decode()}
        self.assertEqual(
            self.request("POST", "/v1/revisions/prepare", envelope, token="x" * 32),
            (401, {"error": "unauthorized"}),
        )
        malformed = [
            b'{"revision":{},"revision":{},"payload":""}',
            json.dumps({**envelope, "extra": "never-log-me"}).encode(),
            json.dumps({"revision": revision, "payload": None}).encode(),
            json.dumps({"revision": revision, "payload": "AB=="}).encode(),
            json.dumps(
                {"revision": revision, "payload": base64.b64encode(b"x" * 129).decode()}
            ).encode(),
        ]
        for raw in malformed:
            with self.subTest(raw=raw[:20]):
                status, body = self.request("POST", "/v1/revisions/prepare", raw=raw)
                self.assertIn(status, (400, 413))
                self.assertNotIn("never-log-me", json.dumps(body))
        self.assertEqual(self.validated, [])

    def test_transition_errors_are_sanitized_and_fixed(self):
        self.start()
        revision = self.revision(1, 1, b"secret")
        self.assertEqual(
            self.request("POST", "/v1/revisions/commit", {"revision": revision}),
            (409, {"error": "revision_rejected"}),
        )
        self.assertEqual(
            self.request("POST", "/v1/revisions/missing", {"secret": "never-log-me"}),
            (404, {"error": "not_found"}),
        )

    def test_reply_before_body_is_read_is_returned(self):
        self.start()
        self.connection_class = LateBodyConnection
        self.assertEqual(
            self.request("POST", "/v1/revisions/missing", {"secret": "never-log-me"}),
            (404, {"error": "not_found"}),
        )
        revision = self.revision(1, 1, b"secret")
        self.assertEqual(
            self.request(
                "POST", "/v1/revisions/commit", {"revision": revision}, token="x" * 32
            ),
            (401, {"error": "unauthorized"}),
        )

    def test_lifecycle_refuses_existing_path_and_fences_receiver(self):
        invalid = (
            (self.path, "short", 128, 1),
            ("relative.sock", TOKEN, 128, 1),
            (self.path, TOKEN, 0, 1),
            (self.path, TOKEN, 128, float("nan")),
        )
        for path, token, limit, timeout in invalid:
            with (
                self.subTest(path=path, limit=limit, timeout=timeout),
                self.assertRaises(ipc.ServerError),
            ):
                ipc.Server(
                    self.receiver,
                    path,
                    token,
                    max_snapshot_bytes=limit,
                    request_timeout=timeout,
                )
        Path(self.path).write_text("do not replace")
        with self.assertRaises(ipc.ServerError):
            self.start()
        self.assertEqual(Path(self.path).read_text(), "do not replace")
        os.remove(self.path)
        self.start()
        with self.assertRaises(ipc.ServerError):
            self.server.start()
        self.server.close()
        self.assertFalse(os.path.exists(self.path))
        with self.assertRaises(receiver.RevisionError):
            self.receiver.readback()
        self.server.close()

    def test_failed_thread_start_is_not_published_to_close(self):
        self.server = ipc.Server(
            self.receiver,
            self.path,
            TOKEN,
            max_snapshot_bytes=128,
            request_timeout=1,
        )
        published = caught = None
        try:
            with (
                mock.patch.object(
                    ipc.threading.Thread,
                    "start",
                    side_effect=RuntimeError("secret-bearing thread failure"),
                ),
                self.assertRaises(ipc.ServerError) as caught,
            ):
                self.server.start()
        finally:
            published = self.server._thread
            self.server._thread = None  # prevent the pre-fix cleanup deadlock
            self.server.close()
        self.assertIsNone(published)
        self.assertNotIn("secret-bearing", str(caught.exception))
        self.assertFalse(os.path.exists(self.path))
        with self.assertRaises(receiver.RevisionError):
            self.receiver.readback()
        self.server.close()


if __name__ == "__main__":
    unittest.main()
