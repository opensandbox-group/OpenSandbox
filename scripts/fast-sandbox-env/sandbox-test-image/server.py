#!/usr/bin/env python3

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

#!/usr/bin/env python3
"""Tiny HTTP workload for the sandbox test image.

Endpoints:
  /health  -> 200 "ok\n"  (healthcheck-style probes)
  /        -> 200 with image/user/process info (probe bodies to assert on)

Binds 0.0.0.0:8080 so tcp:// readiness probes and execd /ping have a real
target inside the sandbox.
"""
import http.server
import os
import socket

PORT = 8080


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            body = b"ok\n"
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        body = (
            f"image={os.environ.get('SANDBOX_TEST_IMAGE', 'unset')} "
            f"version={os.environ.get('SANDBOX_TEST_VERSION', 'unset')} "
            f"user={os.environ.get('USER', 'unknown')} "
            f"host={socket.gethostname()} "
            f"pid={os.getpid()}\n"
        ).encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, fmt, *args):
        print("sandbox-test:", fmt % args, flush=True)


if __name__ == "__main__":
    server = http.server.ThreadingHTTPServer(("0.0.0.0", PORT), Handler)
    print(f"sandbox-test: listening on :{PORT}", flush=True)
    server.serve_forever()
