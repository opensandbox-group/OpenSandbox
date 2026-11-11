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

"""Socket-level regression tests for the Test 13 CONNECT/forward relay."""

import http.client
import http.server
import multiprocessing
import socket
import threading
import unittest

from fast_sandbox_upstream import run_connect_proxy


class EchoHandler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        body = f"{self.path}\nX-Api-Key: {self.headers.get('X-Api-Key', '')}\n".encode()
        self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *_args):
        pass


class ConnectProxyTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.origin = http.server.ThreadingHTTPServer(("127.0.0.1", 0), EchoHandler)
        thread = threading.Thread(target=cls.origin.serve_forever, daemon=True)
        thread.start()

        def stop_origin():
            cls.origin.shutdown()
            cls.origin.server_close()
            thread.join(timeout=5)

        cls.addClassCleanup(stop_origin)
        # Pass a bound socket to avoid a free-port selection/bind race. Spawn
        # exercises the same process boundary on Linux and Windows.
        listener = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        listener.bind(("127.0.0.1", 0))
        listener.listen(64)
        cls.proxy_port = listener.getsockname()[1]
        cls.proxy = multiprocessing.get_context("spawn").Process(
            target=run_connect_proxy, args=(listener,), daemon=True
        )
        try:
            cls.proxy.start()
        finally:
            listener.close()

        def stop_proxy():
            cls.proxy.terminate()
            cls.proxy.join(timeout=5)
            cls.proxy.close()

        cls.addClassCleanup(stop_proxy)

    def relay_count(self):
        conn = http.client.HTTPConnection("127.0.0.1", self.proxy_port, timeout=5)
        try:
            conn.request("GET", "/_count")
            response = conn.getresponse()
            self.assertEqual(response.status, 200)
            return int(response.read().decode().strip().split("=")[1])
        finally:
            conn.close()

    def raw_request(self, request):
        with socket.create_connection(("127.0.0.1", self.proxy_port), timeout=5) as conn:
            conn.sendall(request)
            chunks = []
            while chunk := conn.recv(65536):
                chunks.append(chunk)
            return b"".join(chunks)

    def test_absolute_uri_preserves_path_query_and_credentials(self):
        before = self.relay_count()
        conn = http.client.HTTPConnection("127.0.0.1", self.proxy_port, timeout=5)
        try:
            conn.request(
                "GET",
                f"http://127.0.0.1:{self.origin.server_port}/forward?value=1",
                headers={"X-Api-Key": "test-value"},
            )
            response = conn.getresponse()
            self.assertEqual(response.status, 200)
            self.assertEqual(response.read(), b"/forward?value=1\nX-Api-Key: test-value\n")
        finally:
            conn.close()
        self.assertEqual(self.relay_count(), before + 1)

    def test_connect_relays_bytes_buffered_after_request_head(self):
        before = self.relay_count()
        response = self.raw_request(
            f"CONNECT 127.0.0.1:{self.origin.server_port} HTTP/1.1\r\nHost: origin\r\n\r\n".encode()
            + b"GET /tunnel HTTP/1.1\r\nHost: origin\r\nX-Api-Key: tunnel-value\r\nConnection: close\r\n\r\n"
        )
        self.assertTrue(response.startswith(b"HTTP/1.1 200 Connection established\r\n\r\n"))
        self.assertIn(b"200 OK", response)
        self.assertIn(b"/tunnel\nX-Api-Key: tunnel-value\n", response)
        self.assertEqual(self.relay_count(), before + 1)

    def test_malformed_request_is_closed_with_400(self):
        response = self.raw_request(b"CONNECT\r\n\r\n")
        self.assertTrue(response.startswith(b"HTTP/1.1 400"))

    def test_invalid_port_returns_502_without_killing_relay(self):
        before = self.relay_count()
        response = self.raw_request(b"CONNECT 127.0.0.1:invalid HTTP/1.1\r\n\r\n")
        self.assertTrue(response.startswith(b"HTTP/1.1 502"))
        self.assertEqual(self.relay_count(), before)


if __name__ == "__main__":
    unittest.main()
