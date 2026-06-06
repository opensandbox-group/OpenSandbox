# Copyright 2025 Alibaba Group Holding Ltd.
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
Multi-tenancy e2e tests covering:
- FileTenantProvider: config parsing, hot-reload, auth integration
- HTTPTenantProvider: mock HTTP server, TTL cache, 401 handling
- Auth middleware: multi-tenant mode routing, namespace resolution
- Startup guards: docker + tenants fatal, api_key conflict
"""

import json
import threading
import time
import textwrap
from http.server import HTTPServer, BaseHTTPRequestHandler

import pytest
from fastapi import FastAPI, Request
from fastapi.testclient import TestClient

from opensandbox_server.config import AppConfig, IngressConfig, RuntimeConfig, ServerConfig, TenantsConfig
from opensandbox_server.middleware.auth import AuthMiddleware
from opensandbox_server.tenants import (
    FileTenantProvider,
    HTTPTenantProvider,
    HTTPTenantProviderConfig,
    TenantEntry,
    TenantProviderUnavailable,
    get_current_tenant,
    set_current_tenant,
    validate_tenant_config,
)


# ---------------------------------------------------------------------------
# Fixtures
# ---------------------------------------------------------------------------

@pytest.fixture
def tenants_toml(tmp_path):
    """Write a valid tenants.toml and return its path."""
    content = textwrap.dedent("""\
        [[tenants]]
        name = "team-alpha"
        namespace = "ns-alpha"
        api_keys = ["sk-alpha-1", "sk-alpha-2"]

        [[tenants]]
        name = "team-beta"
        namespace = "ns-beta"
        api_keys = ["sk-beta-1"]
    """)
    path = tmp_path / "tenants.toml"
    path.write_text(content)
    return path


@pytest.fixture
def mock_http_tenant_server():
    """Start a mock HTTP tenant verification server.

    Recognizes:
      - sk-http-valid → namespace=ns-http, ttl=60
      - sk-http-short-ttl → namespace=ns-short, ttl=1
      - anything else → 401
    """
    tenant_db = {
        "sk-http-valid": {"namespace": "ns-http", "ttl": 60},
        "sk-http-short-ttl": {"namespace": "ns-short", "ttl": 1},
    }

    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            api_key = self.headers.get("OPEN-SANDBOX-API-KEY", "")
            if api_key in tenant_db:
                data = tenant_db[api_key]
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps(data).encode())
            else:
                self.send_response(401)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(json.dumps({
                    "code": "UNAUTHORIZED",
                    "message": "Invalid API key",
                }).encode())

        def log_message(self, format, *args):
            pass  # suppress logging

    server = HTTPServer(("127.0.0.1", 0), Handler)
    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    yield f"http://127.0.0.1:{port}"
    server.shutdown()


# ---------------------------------------------------------------------------
# FileTenantProvider tests
# ---------------------------------------------------------------------------

class TestFileTenantProvider:

    def test_load_valid_config(self, tenants_toml):
        provider = FileTenantProvider(tenants_toml)
        provider.start()
        try:
            assert provider.ready()
            assert len(provider.list_tenants()) == 2

            entry = provider.lookup("sk-alpha-1")
            assert entry is not None
            assert entry.name == "team-alpha"
            assert entry.namespace == "ns-alpha"

            entry2 = provider.lookup("sk-alpha-2")
            assert entry2 == entry

            beta = provider.lookup("sk-beta-1")
            assert beta.namespace == "ns-beta"

            assert provider.lookup("nonexistent") is None
        finally:
            provider.close()

    def test_duplicate_keys_rejected(self, tmp_path):
        content = textwrap.dedent("""\
            [[tenants]]
            name = "a"
            namespace = "ns-a"
            api_keys = ["shared-key"]

            [[tenants]]
            name = "b"
            namespace = "ns-b"
            api_keys = ["shared-key"]
        """)
        path = tmp_path / "tenants.toml"
        path.write_text(content)

        provider = FileTenantProvider(path)
        with pytest.raises(ValueError, match="Duplicate api_key"):
            provider.start()

    def test_empty_api_keys_rejected(self, tmp_path):
        content = textwrap.dedent("""\
            [[tenants]]
            name = "empty"
            namespace = "ns-empty"
            api_keys = []
        """)
        path = tmp_path / "tenants.toml"
        path.write_text(content)

        provider = FileTenantProvider(path)
        with pytest.raises(ValueError, match="no api_keys"):
            provider.start()

    def test_file_not_found_raises(self, tmp_path):
        provider = FileTenantProvider(tmp_path / "nonexistent.toml")
        with pytest.raises(FileNotFoundError):
            provider.start()

    def test_hot_reload_new_key(self, tenants_toml):
        provider = FileTenantProvider(tenants_toml)
        provider.start()
        try:
            assert provider.lookup("sk-new") is None

            # Add a new tenant
            new_content = tenants_toml.read_text() + textwrap.dedent("""
                [[tenants]]
                name = "team-new"
                namespace = "ns-new"
                api_keys = ["sk-new"]
            """)
            tenants_toml.write_text(new_content)

            # Wait for watcher to pick up (polls every 2s)
            time.sleep(3)

            entry = provider.lookup("sk-new")
            assert entry is not None
            assert entry.namespace == "ns-new"
        finally:
            provider.close()

    def test_hot_reload_removed_key(self, tenants_toml):
        provider = FileTenantProvider(tenants_toml)
        provider.start()
        try:
            assert provider.lookup("sk-beta-1") is not None

            # Rewrite without team-beta
            content = textwrap.dedent("""\
                [[tenants]]
                name = "team-alpha"
                namespace = "ns-alpha"
                api_keys = ["sk-alpha-1", "sk-alpha-2"]
            """)
            tenants_toml.write_text(content)
            time.sleep(3)

            assert provider.lookup("sk-beta-1") is None
            assert provider.lookup("sk-alpha-1") is not None
        finally:
            provider.close()

    def test_hot_reload_parse_error_keeps_old(self, tenants_toml):
        provider = FileTenantProvider(tenants_toml)
        provider.start()
        try:
            assert provider.lookup("sk-alpha-1") is not None

            # Write invalid TOML
            tenants_toml.write_text("[[[ invalid toml")
            time.sleep(3)

            # Old entries still work
            assert provider.lookup("sk-alpha-1") is not None
        finally:
            provider.close()

    def test_file_delete_clears_all(self, tenants_toml):
        provider = FileTenantProvider(tenants_toml)
        provider.start()
        try:
            assert provider.lookup("sk-alpha-1") is not None
            tenants_toml.unlink()
            time.sleep(3)
            assert provider.lookup("sk-alpha-1") is None
        finally:
            provider.close()


# ---------------------------------------------------------------------------
# HTTPTenantProvider tests
# ---------------------------------------------------------------------------

class TestHTTPTenantProvider:

    def test_lookup_valid_key(self, mock_http_tenant_server):
        provider = HTTPTenantProvider(HTTPTenantProviderConfig(
            endpoint=mock_http_tenant_server,
        ))
        provider.start()
        try:
            entry = provider.lookup("sk-http-valid")
            assert entry is not None
            assert entry.namespace == "ns-http"
        finally:
            provider.close()

    def test_lookup_invalid_key_returns_none(self, mock_http_tenant_server):
        provider = HTTPTenantProvider(HTTPTenantProviderConfig(
            endpoint=mock_http_tenant_server,
        ))
        provider.start()
        try:
            entry = provider.lookup("sk-invalid")
            assert entry is None
        finally:
            provider.close()

    def test_cache_hit_within_ttl(self, mock_http_tenant_server):
        provider = HTTPTenantProvider(HTTPTenantProviderConfig(
            endpoint=mock_http_tenant_server,
        ))
        provider.start()
        try:
            # First call — fetches from server
            entry1 = provider.lookup("sk-http-valid")
            assert entry1 is not None

            # Second call — should be cached (no network)
            entry2 = provider.lookup("sk-http-valid")
            assert entry2 == entry1
        finally:
            provider.close()

    def test_cache_expires_after_ttl(self, mock_http_tenant_server):
        provider = HTTPTenantProvider(HTTPTenantProviderConfig(
            endpoint=mock_http_tenant_server,
        ))
        provider.start()
        try:
            entry = provider.lookup("sk-http-short-ttl")
            assert entry is not None
            assert entry.namespace == "ns-short"

            # Wait for TTL to expire (ttl=1s)
            time.sleep(1.5)

            # Should re-fetch (still valid on server)
            entry2 = provider.lookup("sk-http-short-ttl")
            assert entry2 is not None
            assert entry2.namespace == "ns-short"
        finally:
            provider.close()

    def test_unreachable_endpoint_raises_unavailable(self):
        provider = HTTPTenantProvider(HTTPTenantProviderConfig(
            endpoint="http://127.0.0.1:1",  # unlikely to be listening
            timeout_seconds=0.5,
        ))
        provider.start()
        try:
            with pytest.raises(TenantProviderUnavailable):
                provider.lookup("any-key")
        finally:
            provider.close()

    def test_stale_cache_served_when_endpoint_down(self, mock_http_tenant_server):
        provider = HTTPTenantProvider(HTTPTenantProviderConfig(
            endpoint=mock_http_tenant_server,
            max_stale_seconds=60,
        ))
        provider.start()
        try:
            # Populate cache with short TTL
            entry = provider.lookup("sk-http-short-ttl")
            assert entry is not None

            # Now point to dead endpoint
            provider._config = HTTPTenantProviderConfig(
                endpoint="http://127.0.0.1:1",
                timeout_seconds=0.5,
                max_stale_seconds=60,
            )
            provider._client.close()
            import httpx
            provider._client = httpx.Client(timeout=0.5)

            time.sleep(1.5)  # TTL expires

            # Should serve stale (within max_stale)
            entry2 = provider.lookup("sk-http-short-ttl")
            assert entry2 is not None
            assert entry2.namespace == "ns-short"
        finally:
            provider.close()

    def test_revoked_key_evicted_from_cache(self, mock_http_tenant_server):
        provider = HTTPTenantProvider(HTTPTenantProviderConfig(
            endpoint=mock_http_tenant_server,
        ))
        provider.start()
        try:
            # This key is valid
            entry = provider.lookup("sk-http-valid")
            assert entry is not None

            # Manually expire the cache entry to force re-fetch
            with provider._lock:
                cached = provider._cache["sk-http-valid"]
                provider._cache["sk-http-valid"] = type(cached)(
                    tenant=cached.tenant, fetched_at=0, ttl=0
                )

            # Now mock server doesn't know this key (simulate revocation by using unknown key)
            # We'll test with a key that's cached but server returns 401
            provider._cache["sk-revoked"] = type(cached)(
                tenant=TenantEntry(name="old", namespace="old-ns", api_keys=("sk-revoked",)),
                fetched_at=0,
                ttl=0,
            )

            result = provider.lookup("sk-revoked")
            assert result is None
            assert "sk-revoked" not in provider._cache
        finally:
            provider.close()


# ---------------------------------------------------------------------------
# Auth middleware multi-tenant integration
# ---------------------------------------------------------------------------

class TestAuthMiddlewareMultiTenant:

    def _build_multi_tenant_app(self, provider):
        app = FastAPI()
        config = AppConfig(
            server=ServerConfig(api_key=""),
            runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:latest"),
            ingress=IngressConfig(mode="direct"),
        )
        app.add_middleware(AuthMiddleware, config=config, tenant_provider=provider)

        @app.get("/secured")
        def secured_endpoint(request: Request):
            tenant = get_current_tenant()
            return {
                "tenant_name": tenant.name if tenant else None,
                "tenant_namespace": tenant.namespace if tenant else None,
            }

        return app

    def test_valid_key_resolves_tenant(self, tenants_toml):
        provider = FileTenantProvider(tenants_toml)
        provider.start()
        try:
            app = self._build_multi_tenant_app(provider)
            client = TestClient(app)

            resp = client.get("/secured", headers={"OPEN-SANDBOX-API-KEY": "sk-alpha-1"})
            assert resp.status_code == 200
            data = resp.json()
            assert data["tenant_name"] == "team-alpha"
            assert data["tenant_namespace"] == "ns-alpha"
        finally:
            provider.close()

    def test_invalid_key_returns_401(self, tenants_toml):
        provider = FileTenantProvider(tenants_toml)
        provider.start()
        try:
            app = self._build_multi_tenant_app(provider)
            client = TestClient(app)

            resp = client.get("/secured", headers={"OPEN-SANDBOX-API-KEY": "bad-key"})
            assert resp.status_code == 401
            assert resp.json()["code"] == "INVALID_API_KEY"
        finally:
            provider.close()

    def test_missing_key_returns_401(self, tenants_toml):
        provider = FileTenantProvider(tenants_toml)
        provider.start()
        try:
            app = self._build_multi_tenant_app(provider)
            client = TestClient(app)

            resp = client.get("/secured")
            assert resp.status_code == 401
            assert resp.json()["code"] == "MISSING_API_KEY"
        finally:
            provider.close()

    def test_provider_unavailable_returns_503(self, tenants_toml):
        provider = FileTenantProvider(tenants_toml)
        provider.start()
        try:
            app = self._build_multi_tenant_app(provider)
            client = TestClient(app)

            # Monkey-patch lookup to raise
            def broken_lookup(key):
                raise TenantProviderUnavailable("test")
            provider.lookup = broken_lookup

            resp = client.get("/secured", headers={"OPEN-SANDBOX-API-KEY": "sk-alpha-1"})
            assert resp.status_code == 503
            assert resp.json()["code"] == "TENANT_PROVIDER_UNAVAILABLE"
        finally:
            provider.close()

    def test_http_provider_auth_integration(self, mock_http_tenant_server):
        provider = HTTPTenantProvider(HTTPTenantProviderConfig(
            endpoint=mock_http_tenant_server,
        ))
        provider.start()
        try:
            app = self._build_multi_tenant_app(provider)
            client = TestClient(app)

            resp = client.get("/secured", headers={"OPEN-SANDBOX-API-KEY": "sk-http-valid"})
            assert resp.status_code == 200
            assert resp.json()["tenant_namespace"] == "ns-http"

            resp2 = client.get("/secured", headers={"OPEN-SANDBOX-API-KEY": "bad"})
            assert resp2.status_code == 401
        finally:
            provider.close()


# ---------------------------------------------------------------------------
# Namespace resolution
# ---------------------------------------------------------------------------

class TestNamespaceResolution:

    def test_resolve_namespace_with_tenant(self):
        from opensandbox_server.tenants.context import set_current_tenant, get_current_tenant

        tenant = TenantEntry(name="team-x", namespace="ns-x", api_keys=("k",))
        set_current_tenant(tenant)
        try:
            assert get_current_tenant().namespace == "ns-x"
        finally:
            set_current_tenant(None)

    def test_resolve_namespace_without_tenant(self):
        set_current_tenant(None)
        assert get_current_tenant() is None


# ---------------------------------------------------------------------------
# Startup guards
# ---------------------------------------------------------------------------

class TestStartupGuards:

    def test_tenants_config_with_docker_runtime_rejected(self):
        with pytest.raises(ValueError, match="runtime.type='docker'"):
            validate_tenant_config(runtime_type="docker", api_key="")

    def test_tenants_config_with_api_key_rejected(self):
        with pytest.raises(ValueError, match="server.api_key must be removed"):
            validate_tenant_config(runtime_type="kubernetes", api_key="some-key")

    def test_tenants_config_valid_kubernetes_no_api_key(self):
        validate_tenant_config(runtime_type="kubernetes", api_key="")

    def test_http_provider_requires_endpoint(self):
        with pytest.raises(Exception, match="endpoint must be set"):
            TenantsConfig(provider="http")

    def test_http_provider_with_endpoint_valid(self):
        cfg = TenantsConfig(provider="http", endpoint="http://localhost:8080/verify")
        assert cfg.endpoint == "http://localhost:8080/verify"

    def test_file_provider_no_endpoint_needed(self):
        cfg = TenantsConfig(provider="file")
        assert cfg.endpoint is None
