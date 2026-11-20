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

"""Local protocol tests for the OSEP-0023 credential-bound TLS fixtures.

The guest client (verify/credential-bound-client.py) and the test origin
(verify/credential-bound-origin.py) talk real TLS on 127.0.0.1 with a
throwaway CA: certificate and hostname verification, the closed error
vocabulary, connection identity, the two-phase held request and the refusal
to reconnect. Standard library plus the openssl CLI. These are protocol
fixtures only; they prove nothing about the KVM integration environment.
"""

import hashlib
import importlib.util
import json
import os
import shutil
import socket
import ssl
import subprocess
import sys
import tempfile
import threading
import time
import unittest

HERE = os.path.dirname(os.path.abspath(__file__))
VERIFY_DIR = os.path.join(os.path.dirname(HERE), "verify")
CLIENT_PY = os.path.join(VERIFY_DIR, "credential-bound-client.py")
ORIGIN_PY = os.path.join(VERIFY_DIR, "credential-bound-origin.py")
REPO = os.path.dirname(os.path.dirname(os.path.dirname(HERE)))
WORKFLOW = os.path.join(REPO, ".github", "workflows", "fast-sandbox-test.yml")
HAVE_OPENSSL = shutil.which("openssl") is not None


# --- real TLS fixtures (throwaway CA on 127.0.0.1)

def openssl(*args):
    subprocess.run(["openssl"] + list(args), check=True, capture_output=True)


def make_pki(directory, name):
    ca_key, ca_crt = os.path.join(directory, name + "-ca.key"), os.path.join(directory, name + "-ca.crt")
    cnf = os.path.join(directory, name + ".cnf")
    with open(cnf, "w") as handle:
        handle.write("[req]\ndistinguished_name=dn\nprompt=no\n[dn]\nCN=cbv-test-%s\n"
                     "[v3_ca]\nbasicConstraints=critical,CA:TRUE\nkeyUsage=critical,keyCertSign,cRLSign\n"
                     "[v3_leaf]\nsubjectAltName=DNS:echo.test\nextendedKeyUsage=serverAuth\n" % name)
    openssl("req", "-x509", "-new", "-newkey", "rsa:2048", "-nodes", "-days", "2", "-keyout", ca_key,
            "-out", ca_crt, "-config", cnf, "-extensions", "v3_ca")
    leaf_key, leaf_csr = os.path.join(directory, name + "-leaf.key"), os.path.join(directory, name + "-leaf.csr")
    leaf_crt = os.path.join(directory, name + "-leaf.crt")
    openssl("req", "-new", "-newkey", "rsa:2048", "-nodes", "-keyout", leaf_key, "-out", leaf_csr,
            "-subj", "/CN=echo.test")
    openssl("x509", "-req", "-in", leaf_csr, "-CA", ca_crt, "-CAkey", ca_key, "-CAcreateserial",
            "-out", leaf_crt, "-days", "2", "-extfile", cnf, "-extensions", "v3_leaf")
    return ca_crt, leaf_crt, leaf_key


def load_module(path, name):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def der_sha256(pem_path):
    with open(pem_path) as handle:
        return hashlib.sha256(ssl.PEM_cert_to_DER_cert(handle.read())).hexdigest()


@unittest.skipUnless(HAVE_OPENSSL, "openssl CLI required")
class OriginFixture(unittest.TestCase):
    """A real origin on 127.0.0.1 behind a throwaway CA, plus a client runner."""

    @classmethod
    def setUpClass(cls):
        cls.tmp = tempfile.mkdtemp(prefix="cbv-pki-")
        cls.ca, cls.leaf, cls.key = make_pki(cls.tmp, "main")
        cls.other_ca, _, _ = make_pki(cls.tmp, "other")
        origin = load_module(ORIGIN_PY, "cbv_origin")
        cls.server = origin.build_server(0, cls.leaf, cls.key, host="127.0.0.1")
        cls.port = cls.server.server_address[1]
        cls.thread = threading.Thread(target=cls.server.serve_forever, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.server.shutdown()
        cls.server.server_close()
        shutil.rmtree(cls.tmp, ignore_errors=True)

    def client(self, *args, port=None, cafile=None):
        cmd = [sys.executable, CLIENT_PY] + list(args) + [
            "--host", "echo.test", "--ip", "127.0.0.1", "--port", str(port or self.port),
            "--cafile", cafile or self.ca, "--connect-timeout", "3", "--request-timeout", "5"]
        return subprocess.run(cmd, capture_output=True, text=True, timeout=60)

    @staticmethod
    def parse(stdout):
        lines = [l for l in stdout.splitlines() if l.startswith("CBV-RESULT ")]
        assert len(lines) == 1, stdout
        return json.loads(lines[0][len("CBV-RESULT "):])


class HelperProtocolTest(OriginFixture):
    def test_once_keeps_one_http11_connection(self):
        proc = self.client("once", "--count", "3", "--tag", "t1")
        self.assertEqual(proc.returncode, 0, proc.stderr)
        res = self.parse(proc.stdout)
        self.assertTrue(res["ok"] and res["verified"])
        self.assertEqual(res["connects"], 1)
        self.assertEqual(res["leaf_sha256"], der_sha256(self.leaf))
        conns = {r["origin"]["conn"] for r in res["responses"]}
        self.assertEqual(len(conns), 1)
        self.assertEqual([r["origin"]["req"] for r in res["responses"]], [1, 2, 3])
        self.assertEqual(res["last"]["origin"]["proto"], "HTTP/1.1")
        self.assertEqual(res["last"]["origin"]["cred"], "absent")
        self.assertEqual(res["last"]["origin"]["credential_headers"], [])
        self.assertEqual(res["last"]["origin"]["tag"], "t1")

    def test_origin_reports_digest_never_value(self):
        ctx = ssl.create_default_context(cafile=self.ca)
        with socket.create_connection(("127.0.0.1", self.port), timeout=5) as raw:
            with ctx.wrap_socket(raw, server_hostname="echo.test") as tls:
                tls.sendall(b"GET /cbv HTTP/1.1\r\nHost: echo.test\r\nX-Api-Key: fake-osep23-x\r\n"
                            b"Content-Length: 0\r\n\r\n")
                data = b""
                while b"}" not in data:
                    data += tls.recv(4096)
        self.assertNotIn(b"fake-osep23-x", data)
        body = json.loads(data.split(b"\r\n\r\n", 1)[1])
        self.assertEqual(body["cred"], "sha256:" + hashlib.sha256(b"fake-osep23-x").hexdigest())
        self.assertEqual(body["credential_headers"], ["x-api-key"])

    def test_untrusted_ca_and_wrong_host_fail_verification(self):
        res = self.parse(self.client("once", cafile=self.other_ca).stdout)
        self.assertFalse(res["ok"])
        self.assertEqual((res["error"]["phase"], res["error"]["kind"]), ("handshake", "verify_failed"))
        proc = subprocess.run([sys.executable, CLIENT_PY, "once", "--host", "other.test", "--ip", "127.0.0.1",
                               "--port", str(self.port), "--cafile", self.ca], capture_output=True, text=True)
        self.assertEqual(proc.returncode, 3)
        self.assertEqual(self.parse(proc.stdout)["error"]["kind"], "verify_failed")

    def test_refused_connect_is_classified(self):
        sock = socket.socket()
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        sock.close()
        proc = self.client("once", port=port)
        self.assertEqual(proc.returncode, 3)
        res = self.parse(proc.stdout)
        self.assertEqual((res["error"]["phase"], res["error"]["kind"]), ("connect", "refused"))
        self.assertNotIn("leaf_sha256", res)

    def test_hold_reuses_the_same_socket(self):
        hold = os.path.join(self.tmp, "hold-same")
        proc = subprocess.Popen([sys.executable, CLIENT_PY, "hold", "--dir", hold, "--tag", "h1",
                                 "--host", "echo.test", "--ip", "127.0.0.1", "--port", str(self.port),
                                 "--cafile", self.ca, "--wait-timeout", "30"])
        r1 = wait_json(os.path.join(hold, "r1.json"))
        open(os.path.join(hold, "go"), "w").close()
        self.assertEqual(proc.wait(timeout=30), 0)
        r2 = wait_json(os.path.join(hold, "r2.json"))
        self.assertEqual((r1["connects"], r2["connects"]), (1, 1))
        self.assertEqual(r1["socket_id"], r2["socket_id"])
        self.assertEqual(r1["local_port"], r2["local_port"])
        self.assertEqual(r1["last"]["origin"]["conn"], r2["last"]["origin"]["conn"])
        self.assertEqual(r2["last"]["origin"]["req"], 2)

    def test_hold_reports_peer_close_without_reconnecting(self):
        ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        ctx.load_cert_chain(self.leaf, self.key)
        listener = socket.socket()
        listener.bind(("127.0.0.1", 0))
        listener.listen(4)
        accepted = []

        def serve_one_then_close():
            raw, _ = listener.accept()
            accepted.append(1)
            tls = ctx.wrap_socket(raw, server_side=True)
            data = b""
            while b"\r\n\r\n" not in data:
                data += tls.recv(4096)
            body = b'{"v":1,"conn":"x","req":1,"proto":"HTTP/1.1","cred":"absent","credential_headers":[]}'
            tls.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s" % (len(body), body))
            time.sleep(0.5)
            tls.close()

        threading.Thread(target=serve_one_then_close, daemon=True).start()
        hold = os.path.join(self.tmp, "hold-close")
        proc = subprocess.Popen([sys.executable, CLIENT_PY, "hold", "--dir", hold, "--tag", "h2",
                                 "--host", "echo.test", "--ip", "127.0.0.1",
                                 "--port", str(listener.getsockname()[1]), "--cafile", self.ca,
                                 "--wait-timeout", "30", "--request-timeout", "5"])
        r1 = wait_json(os.path.join(hold, "r1.json"))
        time.sleep(1.0)
        open(os.path.join(hold, "go"), "w").close()
        self.assertEqual(proc.wait(timeout=30), 3)
        listener.close()
        r2 = wait_json(os.path.join(hold, "r2.json"))
        self.assertTrue(r1["ok"])
        self.assertFalse(r2["ok"])
        self.assertTrue(r2["closed_by_peer"], r2["error"])
        self.assertEqual(r2["connects"], 1)
        self.assertEqual(r1["socket_id"], r2["socket_id"])
        self.assertEqual(len(accepted), 1)

    def test_second_connect_is_refused(self):
        client = load_module(CLIENT_PY, "cbv_client_reconnect")
        args = client.parse_args(["once", "--host", "echo.test", "--ip", "127.0.0.1",
                                  "--port", str(self.port), "--cafile", self.ca])
        first = client.Connection(args)
        first.connect()
        first.close()
        with self.assertRaises(client.ProbeError) as raised:
            client.Connection(args).connect()
        self.assertEqual(raised.exception.kind, "reconnect_forbidden")


class WorkflowGuardTest(unittest.TestCase):
    """The hosted job fails, instead of skipping these tests, without openssl."""

    def test_hosted_guard_requires_openssl(self):
        try:
            import yaml  # noqa: PLC0415
        except ImportError:
            self.skipTest("PyYAML not installed")
        bash = shutil.which("bash")
        version = subprocess.run([bash, "-c", "echo ${BASH_VERSINFO[0]}"], capture_output=True, text=True)
        if not version.stdout.strip().isdigit() or int(version.stdout.strip()) < 4:
            self.skipTest("bash >= 4 is required by the guard itself")
        with open(WORKFLOW) as handle:
            job = yaml.safe_load(handle)["jobs"]["verifier-tests"]
        guard = next(s for s in job["steps"] if s.get("name") == "Verify local test prerequisites")
        bin_dir = tempfile.mkdtemp(prefix="cbv-guard-")
        self.addCleanup(shutil.rmtree, bin_dir, True)
        # Every tool the guard may call, except openssl.
        for tool, target in (("bash", bash), ("python", sys.executable), ("git", shutil.which("git")),
                             ("jq", shutil.which("jq"))):
            if target:
                path = os.path.join(bin_dir, tool)
                with open(path, "w") as handle:
                    handle.write("#!/bin/sh\nexec '%s' \"$@\"\n" % target)
                os.chmod(path, 0o755)
        env = dict(os.environ, PATH=bin_dir, **job["env"])
        # GitHub runs `run:` steps with bash -e -o pipefail.
        proc = subprocess.run([bash, "-x", "-e", "-o", "pipefail", "-c", guard["run"]], cwd=REPO, env=env,
                              capture_output=True, text=True)
        self.assertNotEqual(proc.returncode, 0, proc.stderr)
        trace = [line for line in proc.stderr.splitlines() if line.startswith("+ ")]
        self.assertEqual(trace[-1], "+ command -v openssl", proc.stderr)


def wait_json(path, timeout=30):
    deadline = time.time() + timeout
    while time.time() < deadline:
        if os.path.exists(path):
            with open(path) as handle:
                return json.load(handle)
        time.sleep(0.05)
    raise AssertionError("timed out waiting for %s" % path)


if __name__ == "__main__":
    unittest.main()
