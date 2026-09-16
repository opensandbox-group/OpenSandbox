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

"""Live-addon lifecycle tests for the OSEP-0023 revision receiver."""

import base64
import hashlib
import http.client
import importlib.util
import json
import os
import shutil
import socket
import stat
import subprocess
import sys
import tempfile
import time
import types
import unittest
from pathlib import Path
from unittest import mock

MITMSCRIPTS = Path(__file__).resolve().parents[1] / "mitmscripts"
TOKEN = "0123456789abcdef0123456789abcdef"
MITMDUMP = shutil.which("mitmdump")
ENV = {
    "socket": "OPENSANDBOX_EGRESS_REVISION_IPC_SOCKET",
    "token": "OPENSANDBOX_EGRESS_REVISION_IPC_TOKEN",
    "control": "OPENSANDBOX_EGRESS_REVISION_CONTROL_GENERATION",
    "subject": "OPENSANDBOX_EGRESS_REVISION_SUBJECT_GENERATION",
    "limit": "OPENSANDBOX_EGRESS_REVISION_MAX_SNAPSHOT_BYTES",
}


class _Log:
    def __init__(self):
        self.messages = []

    def warn(self, message):
        self.messages.append(message)

    def info(self, message):
        self.messages.append(message)


def load_system():
    mitmproxy = types.ModuleType("mitmproxy")
    mitmproxy.ctx = types.SimpleNamespace(
        log=_Log(), options=types.SimpleNamespace(ignore_hosts=[], ssl_insecure=False)
    )
    mitmproxy.http = types.SimpleNamespace(HTTPFlow=object)
    mitmproxy_tls = types.ModuleType("mitmproxy.tls")
    mitmproxy_tls.ClientHelloData = object
    sys.modules["mitmproxy"] = mitmproxy
    sys.modules["mitmproxy.tls"] = mitmproxy_tls
    spec = importlib.util.spec_from_file_location(
        "opensandbox_revision_runtime", MITMSCRIPTS / "system.py"
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


class UnixConnection(http.client.HTTPConnection):
    def __init__(self, path):
        super().__init__("revision-ipc", timeout=2)
        self.path = path

    def connect(self):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.settimeout(self.timeout)
        self.sock.connect(self.path)


def decision_payload():
    return json.dumps(
        {
            "version": 1,
            "vaultRevision": 0,
            "effectivePolicyEpoch": 0,
            "interceptionMode": "credential-bound",
            "state": "active-empty",
            "tlsBindingHostSelectors": [],
            "fullRenderedBindings": [],
            "redactions": [],
        },
        separators=(",", ":"),
    ).encode()


def revision(payload):
    return {
        "controlGeneration": "control-a",
        "subjectGeneration": "subject-a",
        "decisionEpoch": 1,
        "vaultRevision": 0,
        "policyEpoch": 0,
        "digest": hashlib.sha256(payload).hexdigest(),
    }


def request(path, method, target, value=None):
    body = None if value is None else json.dumps(value)
    headers = {"Authorization": f"Bearer {TOKEN}"}
    if body is not None:
        headers["Content-Type"] = "application/json"
    connection = UnixConnection(path)
    connection.request(method, target, body=body, headers=headers)
    response = connection.getresponse()
    result = response.status, json.loads(response.read())
    connection.close()
    return result


class RevisionRuntimeTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory(prefix="osrr-", dir="/tmp")
        self.path = os.path.join(self.directory.name, "receiver.sock")
        self.system = None
        sys.path.insert(0, str(MITMSCRIPTS))

    def tearDown(self):
        if self.system is not None:
            self.system.done()
        sys.path.remove(str(MITMSCRIPTS))
        self.directory.cleanup()

    def configured_env(self):
        return {
            ENV["socket"]: self.path,
            ENV["token"]: TOKEN,
            ENV["control"]: "control-a",
            ENV["subject"]: "subject-a",
            ENV["limit"]: "4096",
        }

    def request(self, method, target, value=None):
        return request(self.path, method, target, value)

    def test_missing_configuration_keeps_runtime_disabled(self):
        with mock.patch.dict(os.environ, {}, clear=True):
            self.system = load_system()
            self.system.load(None)
        self.assertIsNone(self.system._revision_server)
        self.assertIsNone(self.system._revision_receiver)
        self.assertFalse(os.path.exists(self.path))

    def test_partial_or_invalid_configuration_fails_without_secrets(self):
        cases = (
            {ENV["token"]: TOKEN},
            {**self.configured_env(), ENV["limit"]: "04096"},
            {**self.configured_env(), ENV["limit"]: str(2**63)},
            {**self.configured_env(), ENV["limit"]: "9" * 5000},
        )
        for values in cases:
            with self.subTest(values=set(values)):
                with mock.patch.dict(os.environ, values, clear=True):
                    self.system = load_system()
                    with self.assertRaises(SystemExit) as caught:
                        self.system.load(None)
                self.assertEqual(
                    str(caught.exception),
                    "credential proxy: invalid revision runtime configuration",
                )
                self.assertNotIn(TOKEN, repr(caught.exception))

    def test_complete_configuration_serves_validated_revision_until_done(self):
        with mock.patch.dict(os.environ, self.configured_env(), clear=True):
            self.system = load_system()
            self.system.load(None)
        self.assertEqual(stat.S_IMODE(os.stat(self.path).st_mode), 0o600)

        payload = decision_payload()
        identity = revision(payload)
        self.assertEqual(
            self.request(
                "POST",
                "/v1/revisions/prepare",
                {
                    "revision": identity,
                    "payload": base64.b64encode(payload).decode(),
                },
            ),
            (200, {"revision": identity}),
        )
        self.assertEqual(
            self.request("POST", "/v1/revisions/commit", {"revision": identity}),
            (200, {"revision": identity}),
        )
        self.assertEqual(
            self.request("GET", "/v1/revisions/active"),
            (200, {"revision": identity}),
        )
        self.assertEqual(self.system._revision_receiver.acquire().payload, payload)
        with self.assertRaisesRegex(
            SystemExit, "invalid revision runtime configuration"
        ):
            self.system.load(None)

        receiver = self.system._revision_receiver
        revision_error = receiver.readback.__func__.__globals__["RevisionError"]
        self.system.done()
        self.assertIsNone(self.system._revision_server)
        self.assertIsNone(self.system._revision_receiver)
        self.assertFalse(os.path.exists(self.path))
        with self.assertRaises(revision_error):
            receiver.readback()
        self.system.done()


@unittest.skipUnless(MITMDUMP, "mitmdump is not installed")
class RealMitmproxyRevisionRuntimeTest(unittest.TestCase):
    def test_partial_configuration_exits_nonzero_without_token(self):
        env = {
            **os.environ,
            ENV["token"]: TOKEN,
        }
        process = subprocess.Popen(
            [
                MITMDUMP,
                "--listen-host",
                "127.0.0.1",
                "--listen-port",
                "0",
                "-s",
                str(MITMSCRIPTS / "system.py"),
                "--set",
                "termlog_verbosity=info",
            ],
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
        )
        try:
            output = process.communicate(timeout=5)[0]
        except subprocess.TimeoutExpired:
            process.terminate()
            output = process.communicate(timeout=10)[0]
            self.fail(f"mitmdump accepted partial revision configuration: {output}")
        self.assertNotEqual(process.returncode, 0)
        self.assertIn(
            "credential proxy: invalid revision runtime configuration", output
        )
        self.assertNotIn("Addon error:", output)
        self.assertNotIn("Traceback", output)
        self.assertNotIn("proxy listening", output.lower())
        self.assertNotIn(TOKEN, output)

    def test_process_loads_receiver_and_removes_socket_on_exit(self):
        with tempfile.TemporaryDirectory(prefix="osrr-real-", dir="/tmp") as directory:
            socket_path = os.path.join(directory, "receiver.sock")
            env = {
                **os.environ,
                ENV["socket"]: socket_path,
                ENV["token"]: TOKEN,
                ENV["control"]: "control-a",
                ENV["subject"]: "subject-a",
                ENV["limit"]: "4096",
            }
            process = subprocess.Popen(
                [
                    MITMDUMP,
                    "--listen-host",
                    "127.0.0.1",
                    "--listen-port",
                    "0",
                    "-s",
                    str(MITMSCRIPTS / "system.py"),
                    "--set",
                    "termlog_verbosity=info",
                ],
                env=env,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
            )
            output = ""
            try:
                deadline = time.monotonic() + 15
                while time.monotonic() < deadline and not os.path.exists(socket_path):
                    if process.poll() is not None:
                        output = process.stdout.read()
                        self.fail(f"mitmdump exited before receiver startup: {output}")
                    time.sleep(0.05)
                self.assertTrue(os.path.exists(socket_path))

                payload = decision_payload()
                identity = revision(payload)
                self.assertEqual(
                    request(
                        socket_path,
                        "POST",
                        "/v1/revisions/prepare",
                        {
                            "revision": identity,
                            "payload": base64.b64encode(payload).decode(),
                        },
                    ),
                    (200, {"revision": identity}),
                )
                self.assertEqual(
                    request(
                        socket_path,
                        "POST",
                        "/v1/revisions/commit",
                        {"revision": identity},
                    ),
                    (200, {"revision": identity}),
                )
            finally:
                if process.poll() is None:
                    process.terminate()
                try:
                    output += process.communicate(timeout=10)[0]
                except subprocess.TimeoutExpired:
                    process.kill()
                    output += process.communicate(timeout=5)[0]
            self.assertFalse(os.path.exists(socket_path))
            self.assertNotIn(TOKEN, output)


if __name__ == "__main__":
    unittest.main()
