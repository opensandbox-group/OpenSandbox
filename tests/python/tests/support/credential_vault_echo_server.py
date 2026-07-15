#
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
#
"""Small HTTP target used by Credential Vault E2E tests."""

from __future__ import annotations

import json
import os
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

EXPECTED_HEADERS: dict[str, dict[str, str]] = {
    "/bearer": {
        "authorization": "Bearer vault-bearer-token",
    },
    "/basic": {
        "authorization": "Basic dXNlcjpwYXNz",
    },
    "/api-key": {
        "x-api-key": "vault-api-key-token",
    },
    "/custom-headers": {
        "x-client-id": "vault-client-id",
        "x-client-secret": "vault-client-secret",
    },
    "/runtime-added": {
        "x-runtime-token": "vault-runtime-token",
    },
    "/runtime-replaced": {
        "x-runtime-token": "vault-runtime-token-replaced",
    },
}


class CredentialVaultEchoHandler(BaseHTTPRequestHandler):
    server_version = "OpenSandboxCredentialVaultE2E/1.0"

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        parsed = urlsplit(self.path)
        route = parsed.path
        if route == "/healthz":
            self._write_json(HTTPStatus.OK, {"status": "ok"})
            return

        if route == "/query-substitution":
            query = parse_qs(parsed.query, keep_blank_values=True)
            ok = query.get("api_key") == ["vault-query-secret"]
            self._write_json(
                HTTPStatus.OK if ok else HTTPStatus.UNAUTHORIZED,
                {
                    "ok": ok,
                    "case": "query-substitution",
                    "query": query,
                    "missingOrInvalid": [] if ok else ["api_key"],
                },
            )
            return

        if route == "/tenant/vault-path-secret/resource":
            query = parse_qs(parsed.query, keep_blank_values=True)
            ok = query.get("tenant") == ["__path_secret__"]
            self._write_json(
                HTTPStatus.OK if ok else HTTPStatus.UNAUTHORIZED,
                {
                    "ok": ok,
                    "case": "path-substitution",
                    "queryStillPlaceholder": ok,
                    "missingOrInvalid": [] if ok else ["tenant"],
                },
            )
            return

        expected = EXPECTED_HEADERS.get(route)
        if expected is None:
            self._write_json(HTTPStatus.NOT_FOUND, {"ok": False, "error": "unknown path"})
            return

        received = {name.lower(): value for name, value in self.headers.items()}
        mismatches = [
            name
            for name, expected_value in expected.items()
            if received.get(name) != expected_value
        ]
        self._write_json(
            HTTPStatus.UNAUTHORIZED if mismatches else HTTPStatus.OK,
            {
                "ok": not mismatches,
                "case": route.lstrip("/"),
                "validatedHeaders": sorted(expected),
                "missingOrInvalid": mismatches,
            },
        )

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler API
        parsed = urlsplit(self.path)
        if parsed.path != "/body-substitution":
            self._write_json(HTTPStatus.NOT_FOUND, {"ok": False, "error": "unknown path"})
            return

        length = int(self.headers.get("content-length", "0") or "0")
        raw_body = self.rfile.read(length)
        try:
            body = json.loads(raw_body.decode("utf-8"))
        except (json.JSONDecodeError, UnicodeDecodeError):
            self._write_json(HTTPStatus.BAD_REQUEST, {"ok": False, "error": "invalid json"})
            return

        ok = body.get("client_secret") == "vault-body-secret"
        self._write_json(
            HTTPStatus.OK if ok else HTTPStatus.UNAUTHORIZED,
            {
                "ok": ok,
                "case": "body-substitution",
                "missingOrInvalid": [] if ok else ["client_secret"],
                "body": body,
            },
        )

    def log_message(self, _format: str, *_args: object) -> None:
        return

    def _write_json(self, status: HTTPStatus, body: dict[str, object]) -> None:
        payload = json.dumps(body, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("content-type", "application/json")
        self.send_header("content-length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def main() -> None:
    host = os.getenv("OPENSANDBOX_CREDENTIAL_VAULT_ECHO_HOST", "0.0.0.0")
    port = int(os.getenv("OPENSANDBOX_CREDENTIAL_VAULT_ECHO_PORT", "80"))
    ThreadingHTTPServer((host, port), CredentialVaultEchoHandler).serve_forever()


if __name__ == "__main__":
    main()
