#!/usr/bin/env python3

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

"""
Lightweight HTTP server to receive OPENSANDBOX_EGRESS_DENY_WEBHOOK callbacks.

Config:
- WEBHOOK_HOST: listen address (default 0.0.0.0)
- WEBHOOK_PORT: listen port (default 8000)
- WEBHOOK_PATH: webhook path (default /)

Run:
  python webhook_server.py
Then point OPENSANDBOX_EGRESS_DENY_WEBHOOK to http://<host>:<port><path>
"""

import http.server
import json
import os
import socketserver
from datetime import datetime

HOST = os.getenv("WEBHOOK_HOST", "0.0.0.0")
PORT = int(os.getenv("WEBHOOK_PORT", "8000"))
PATH = os.getenv("WEBHOOK_PATH", "/")


class WebhookHandler(http.server.BaseHTTPRequestHandler):
    def _send(self, code: int = 200, body: str = "ok") -> None:
        self.send_response(code)
        self.send_header("Content-Type", "text/plain; charset=utf-8")
        self.end_headers()
        self.wfile.write(body.encode("utf-8"))

    def do_POST(self) -> None:  # noqa: N802 (BaseHTTPRequestHandler API)
        # Only allow the configured path
        if self.path != PATH:
            self._send(404, "not found")
            return

        length = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(length) if length else b""

        payload = raw.decode("utf-8", errors="replace")
        try:
            parsed = json.loads(payload)
        except json.JSONDecodeError:
            parsed = None

        # Log request info for debugging
        print(f"\n[{datetime.utcnow().isoformat()}Z] Received webhook")
        print(f"Path: {self.path}")
        print(f"Headers: {dict(self.headers)}")
        print(f"Raw body: {payload}")
        if parsed is not None:
            print("Parsed JSON:")
            print(json.dumps(parsed, indent=2))

        self._send(200, "received")

    # Silence default logging to reduce noise
    def log_message(self, *args) -> None:
        return


def main() -> None:
    with socketserver.TCPServer((HOST, PORT), WebhookHandler) as httpd:
        print(f"Listening on http://{HOST}:{PORT}{PATH} ...")
        httpd.serve_forever()


if __name__ == "__main__":
    main()