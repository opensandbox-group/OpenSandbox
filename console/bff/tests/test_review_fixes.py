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
from app.session import admin_session, encode_session, tenant_session  # noqa: E402
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


class AggregateStatsTests(unittest.TestCase):
    def test_skips_missing_wall_clock_and_terminal_expiry(self) -> None:
        from datetime import datetime, timezone

        from app.runtime import aggregate_runtime_stats

        now = datetime(2026, 1, 1, tzinfo=timezone.utc)
        stats = aggregate_runtime_stats(
            [
                {
                    "status": {"state": "Running"},
                    "runtimeSummary": {
                        "wallClockSeconds": 10,
                        "remainingSeconds": 10,
                        "asOf": "2026-01-01T00:00:00Z",
                    },
                },
                {
                    "status": {"state": "Running"},
                    "runtimeSummary": {"remainingSeconds": 10000, "asOf": "2026-01-01T00:00:00Z"},
                },
                {
                    "status": {"state": "Terminated"},
                    "runtimeSummary": {
                        "wallClockSeconds": 5,
                        "remainingSeconds": 1,
                        "asOf": "2026-01-01T00:00:00Z",
                    },
                },
            ],
            now=now,
        )
        self.assertEqual(stats["totalWallClockSeconds"], 15)
        self.assertEqual(stats["avgWallClockSeconds"], 7)
        self.assertEqual(stats["expiringWithin30mCount"], 1)
        self.assertEqual(stats["asOf"], "2026-01-01T00:00:00Z")


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


def _admin_cookie() -> str:
    return encode_session(get_settings(), admin_session())


class AdminReviewTests(unittest.TestCase):
    def test_tenants_route_returns_names_only(self) -> None:
        name = get_settings().bff_session_cookie_name
        with TestClient(app) as client:
            client.cookies.set(name, _admin_cookie())
            listed = client.get("/api/admin/tenants")
            self.assertEqual(listed.status_code, 200)
            self.assertEqual(listed.json()["items"], [{"name": "acme", "namespace": "ns-acme"}])
            self.assertNotIn("api_key", listed.text)
            self.assertNotIn("key-one", listed.text)

            client.cookies.set(name, _token())
            denied = client.get("/api/admin/tenants")
            self.assertEqual(denied.status_code, 403)

    def test_unknown_tenant_and_bad_page_are_400(self) -> None:
        name = get_settings().bff_session_cookie_name
        with TestClient(app) as client:
            client.cookies.set(name, _admin_cookie())
            unknown = client.get("/api/admin/sandboxes", params={"tenant": "missing"})
            self.assertEqual(unknown.status_code, 400)
            self.assertEqual(unknown.json()["detail"]["code"], "UNKNOWN_TENANT")

            bad_page = client.get("/api/admin/sandboxes", params={"page": "abc"})
            self.assertEqual(bad_page.status_code, 400)
            self.assertEqual(bad_page.json()["detail"]["code"], "INVALID_QUERY")

    def test_timeout_on_one_tenant_keeps_the_other(self) -> None:
        import httpx

        from app import lifecycle

        path = Path(_TMP.name) / "two-tenants.toml"
        path.write_text(
            """
[[tenants]]
name = "acme"
namespace = "ns-acme"
api_keys = ["key-one"]

[[tenants]]
name = "beta"
namespace = "ns-beta"
api_keys = ["key-beta"]
""".lstrip(),
            encoding="utf-8",
        )

        async def request(method, url, **kwargs):  # type: ignore[no-untyped-def]
            key = (kwargs.get("headers") or {}).get("OPEN-SANDBOX-API-KEY")
            if key == "key-one":
                raise httpx.TimeoutException("slow")
            return httpx.Response(
                200,
                json={
                    "items": [{"id": "beta-1", "status": {"state": "Running"}}],
                    "pagination": {"hasNextPage": False},
                },
            )

        settings = get_settings()
        name = settings.bff_session_cookie_name
        with unittest.mock.patch.object(settings, "tenants_toml_path", str(path)):
            clear_tenant_cache()
            with TestClient(app) as client:
                client.cookies.set(name, _admin_cookie())
                with unittest.mock.patch.object(lifecycle._shared_client, "request", request):
                    listed = client.get("/api/admin/sandboxes")
        clear_tenant_cache()
        self.assertEqual(listed.status_code, 200)
        body = listed.json()
        self.assertEqual([item["id"] for item in body["items"]], ["beta-1"])
        self.assertEqual(body["pagination"]["tenantErrors"][0]["tenant"], "acme")
        self.assertEqual(body["pagination"]["tenantErrors"][0]["code"], "UPSTREAM_TIMEOUT")
        self.assertFalse(body["pagination"]["capped"])

    def test_runtime_stats_walks_every_upstream_page(self) -> None:
        import httpx

        from app import lifecycle

        async def request(method, url, **kwargs):  # type: ignore[no-untyped-def]
            page = int((kwargs.get("params") or {}).get("page") or 1)
            if page == 1:
                items = [{"id": f"s{i}", "status": {"state": "Running"}} for i in range(200)]
                return httpx.Response(200, json={"items": items, "pagination": {"hasNextPage": True}})
            self.assertEqual(page, 2)
            return httpx.Response(
                200,
                json={
                    "items": [{"id": "last", "status": {"state": "Running"}}],
                    "pagination": {"hasNextPage": False},
                },
            )

        name = get_settings().bff_session_cookie_name
        with TestClient(app) as client:
            client.cookies.set(name, _admin_cookie())
            with unittest.mock.patch.object(lifecycle._shared_client, "request", request):
                stats = client.get("/api/admin/stats/runtime")
        self.assertEqual(stats.status_code, 200)
        self.assertEqual(stats.json()["runningCount"], 201)

    def test_pool_proxy_falls_back_when_the_first_key_is_rejected(self) -> None:
        import httpx

        from app import lifecycle

        path = Path(_TMP.name) / "pool-tenants.toml"
        path.write_text(
            """
[[tenants]]
name = "old"
namespace = "ns-old"
api_keys = ["revoked"]

[[tenants]]
name = "new"
namespace = "ns-new"
api_keys = ["live"]
""".lstrip(),
            encoding="utf-8",
        )
        seen: list[str] = []

        async def request(method, url, **kwargs):  # type: ignore[no-untyped-def]
            key = (kwargs.get("headers") or {}).get("OPEN-SANDBOX-API-KEY")
            seen.append(str(key))
            if key == "revoked":
                return httpx.Response(401, json={"code": "UNAUTHORIZED", "message": "revoked"})
            return httpx.Response(200, json={"items": [{"name": "pool-a"}]})

        settings = get_settings()
        name = settings.bff_session_cookie_name
        with unittest.mock.patch.object(settings, "tenants_toml_path", str(path)):
            clear_tenant_cache()
            with TestClient(app) as client:
                client.cookies.set(name, _admin_cookie())
                with unittest.mock.patch.object(lifecycle._shared_client, "request", request):
                    listed = client.get("/api/pools")
        clear_tenant_cache()
        self.assertEqual(listed.status_code, 200)
        self.assertEqual(listed.json()["items"][0]["name"], "pool-a")
        self.assertEqual(seen, ["revoked", "live"])


if __name__ == "__main__":
    unittest.main()
