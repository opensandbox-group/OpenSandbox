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

from __future__ import annotations

import os
import subprocess
import tempfile
import unittest
import unittest.mock
from pathlib import Path
from urllib.parse import quote

_TMP = tempfile.TemporaryDirectory()
_TENANTS = Path(_TMP.name) / "tenants.toml"
_TENANTS.write_text(
    """
[[tenants]]
name = "acme"
namespace = "ns-acme"
api_keys = ["key-one", "key-two"]
""".lstrip(),
    encoding="utf-8",
)
os.environ["TENANTS_TOML_PATH"] = str(_TENANTS)
os.environ["BFF_SESSION_SECRET"] = "test-session-secret-not-for-prod"
os.environ["BFF_ADMIN_TOKEN"] = "test-admin-token"
os.environ["LIFECYCLE_API_BASE"] = "http://127.0.0.1:9/v1"
os.environ["BFF_HTTP_TIMEOUT_SECONDS"] = "1"

from fastapi import HTTPException  # noqa: E402
from fastapi.testclient import TestClient  # noqa: E402

from app.config import get_settings  # noqa: E402
from app.lifecycle import LifecycleClient, normalize_upstream_path  # noqa: E402
from app.session import encode_session, tenant_session  # noqa: E402
from app.tenants import clear_tenant_cache, load_tenants, lookup_by_api_key  # noqa: E402

get_settings.cache_clear()
clear_tenant_cache()

from app.main import app  # noqa: E402


def _token() -> str:
    return encode_session(get_settings(), tenant_session("acme", "ns-acme"))


class SessionCookieTests(unittest.TestCase):
    def test_query_parameter_never_authenticates(self) -> None:
        token = _token()
        name = get_settings().bff_session_cookie_name
        with TestClient(app) as client:
            queried = client.get("/api/sandboxes", params={"cookie": token})
            self.assertEqual(queried.status_code, 401)

            from app import lifecycle

            async def ok(method, url, **kwargs):  # type: ignore[no-untyped-def]
                import httpx

                return httpx.Response(200, json={"items": [], "pagination": {"totalItems": 0}})

            client.cookies.set(name, token)
            with unittest.mock.patch.object(lifecycle._shared_client, "request", ok):
                authed = client.get("/api/sandboxes")
            self.assertEqual(authed.status_code, 200)


class UpstreamPathTests(unittest.TestCase):
    def test_quotes_reserved_characters_and_rejects_dot_segments(self) -> None:
        self.assertEqual(
            normalize_upstream_path("/sandboxes/abc?x"),
            "/sandboxes/" + quote("abc?x", safe=""),
        )
        with self.assertRaises(HTTPException) as ctx:
            normalize_upstream_path("/sandboxes/..")
        self.assertEqual(ctx.exception.status_code, 400)

    def test_route_rejects_parent_segment(self) -> None:
        name = get_settings().bff_session_cookie_name
        with TestClient(app) as client:
            client.cookies.set(name, _token())
            res = client.get("/api/sandboxes/%2E%2E")
        self.assertEqual(res.status_code, 400)
        self.assertEqual(res.json()["detail"]["code"], "INVALID_PATH")


class UpstreamTransportTests(unittest.TestCase):
    def test_timeout_and_connect_errors_are_gateway_status(self) -> None:
        import httpx

        from app import lifecycle

        name = get_settings().bff_session_cookie_name
        with TestClient(app) as client:
            client.cookies.set(name, _token())
            with unittest.mock.patch.object(
                lifecycle._shared_client,
                "request",
                side_effect=httpx.TimeoutException("slow"),
            ):
                timed_out = client.get("/api/sandboxes")
            self.assertEqual(timed_out.status_code, 504)
            self.assertEqual(timed_out.json()["detail"]["code"], "UPSTREAM_TIMEOUT")

            with unittest.mock.patch.object(
                lifecycle._shared_client,
                "request",
                side_effect=httpx.ConnectError("down"),
            ):
                down = client.get("/api/sandboxes")
            self.assertEqual(down.status_code, 502)
            self.assertEqual(down.json()["detail"]["code"], "UPSTREAM_UNAVAILABLE")


class TenantLoadTests(unittest.TestCase):
    def test_second_api_key_matches_and_duplicate_is_unavailable(self) -> None:
        record = lookup_by_api_key(_TENANTS, "key-two")
        self.assertIsNotNone(record)
        assert record is not None
        self.assertEqual(record.api_key, "key-one")

        dup = Path(_TMP.name) / "dup.toml"
        dup.write_text(
            """
[[tenants]]
name = "a"
namespace = "na"
api_keys = ["same"]

[[tenants]]
name = "b"
namespace = "nb"
api_keys = ["same"]
""".lstrip(),
            encoding="utf-8",
        )
        with self.assertRaises(HTTPException) as ctx:
            load_tenants(dup)
        self.assertEqual(ctx.exception.status_code, 503)

    def test_shared_client_is_reused(self) -> None:
        from app import lifecycle

        with TestClient(app):
            self.assertIsNotNone(lifecycle._shared_client)
            first = lifecycle._shared_client
            client = LifecycleClient(get_settings())
            opened, owns = client._open_client()
            self.assertIs(opened, first)
            self.assertFalse(owns)


class ExtractTenantsTests(unittest.TestCase):
    def test_blank_line_keeps_later_tenants(self) -> None:
        src = Path(_TMP.name) / "cm.yaml"
        out = Path(_TMP.name) / "out.toml"
        src.write_text(
            """
apiVersion: v1
kind: ConfigMap
data:
  tenants.toml: |
    [[tenants]]
    name = "a"
    namespace = "na"
    api_keys = ["k1"]

    [[tenants]]
    name = "b"
    namespace = "nb"
    api_keys = ["k2"]
kind: ConfigMap
""".lstrip(),
            encoding="utf-8",
        )
        script = Path(__file__).resolve().parents[2] / "scripts" / "extract-tenants-toml.sh"
        subprocess.run(["bash", str(script), str(src), str(out)], check=True)
        text = out.read_text(encoding="utf-8")
        self.assertIn('name = "b"', text)
        self.assertIn('api_keys = ["k2"]', text)


if __name__ == "__main__":
    unittest.main()
