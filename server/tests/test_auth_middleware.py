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

from fastapi import FastAPI
from fastapi.testclient import TestClient

from opensandbox_server.config import AppConfig, IngressConfig, RuntimeConfig, ServerConfig
from opensandbox_server.middleware.auth import AuthMiddleware


def _app_config_with_api_key() -> AppConfig:
    return AppConfig(
        server=ServerConfig(api_key="secret-key"),
        runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:latest"),
        ingress=IngressConfig(mode="direct"),
    )


def _build_test_app():
    app = FastAPI()
    config = _app_config_with_api_key()
    app.add_middleware(AuthMiddleware, config=config)

    @app.get("/secured")
    def secured_endpoint():
        return {"ok": True}

    return app


def test_auth_middleware_rejects_missing_key():
    app = _build_test_app()
    client = TestClient(app)
    response = client.get("/secured")
    assert response.status_code == 401
    assert response.json()["code"] == "MISSING_API_KEY"


def test_auth_middleware_accepts_valid_key():
    app = _build_test_app()
    client = TestClient(app)
    response = client.get("/secured", headers={"OPEN-SANDBOX-API-KEY": "secret-key"})
    assert response.status_code == 200
    assert response.json() == {"ok": True}


def test_auth_middleware_skips_validation_for_proxy_to_sandbox():
    """Proxy-to-sandbox paths must not require API key; server only forwards to sandbox."""
    app = _build_test_app()

    @app.get("/sandboxes/{sandbox_id}/proxy/{port}/{full_path:path}")
    def proxy_echo(sandbox_id: str, port: int, full_path: str):
        return {"proxied": True, "sandbox_id": sandbox_id, "port": port, "path": full_path}

    client = TestClient(app)
    # No OPEN-SANDBOX-API-KEY header; should still succeed for proxy path
    response = client.get("/sandboxes/abc-123/proxy/8080/foo/bar")
    assert response.status_code == 200
    assert response.json()["proxied"] is True
    assert response.json()["sandbox_id"] == "abc-123"
    assert response.json()["port"] == 8080
    assert response.json()["path"] == "foo/bar"


def test_auth_middleware_v1_proxy_path_exempt():
    """V1 prefix proxy path is also exempt."""
    app = _build_test_app()

    @app.get("/v1/sandboxes/{sandbox_id}/proxy/{port}/{full_path:path}")
    def proxy_echo(sandbox_id: str, port: int, full_path: str):
        return {"proxied": True}

    client = TestClient(app)
    response = client.get("/v1/sandboxes/sid/proxy/443/")
    assert response.status_code == 200
    assert response.json()["proxied"] is True


def test_auth_middleware_requires_key_for_non_proxy_paths_containing_proxy_and_sandboxes():
    """Paths that contain both 'proxy' and 'sandboxes' but not in proxy-route shape still require auth."""
    app = _build_test_app()

    @app.get("/proxy/sandboxes/anything")
    def fake_proxy():
        return {"reached": True}

    client = TestClient(app)
    response = client.get("/proxy/sandboxes/anything")
    assert response.status_code == 401
    assert response.json()["code"] == "MISSING_API_KEY"


def test_auth_middleware_requires_key_for_malformed_proxy_port():
    """Malformed port (non-numeric) must get 401, not 422; limits unauthenticated surface."""
    app = _build_test_app()

    @app.get("/sandboxes/{sandbox_id}/proxy/{port}/{full_path:path}")
    def proxy_echo(sandbox_id: str, port: int, full_path: str):
        return {"proxied": True}

    client = TestClient(app)
    response = client.get("/sandboxes/s1/proxy/not-a-port/x")
    assert response.status_code == 401
    assert response.json()["code"] == "MISSING_API_KEY"


def test_auth_middleware_is_proxy_path_rejects_traversal():
    """Paths containing '..' are never considered proxy (no auth bypass)."""
    assert AuthMiddleware._is_proxy_path("/sandboxes/abc/proxy/8080/../other") is False
    assert AuthMiddleware._is_proxy_path("/sandboxes/../admin/proxy/8080") is False


def test_auth_middleware_is_proxy_path_accepts_valid_shapes():
    """Only exact proxy route shape (including numeric port) is accepted."""
    assert AuthMiddleware._is_proxy_path("/sandboxes/id/proxy/8080") is True
    assert AuthMiddleware._is_proxy_path("/sandboxes/id/proxy/8080/") is True
    assert AuthMiddleware._is_proxy_path("/v1/sandboxes/id/proxy/443/path") is True
    assert AuthMiddleware._is_proxy_path("/proxy/sandboxes/x") is False
    assert AuthMiddleware._is_proxy_path("/foo/sandboxes/id/proxy/8080") is False
    # Non-numeric port must not skip auth (malformed path → 401, not 422)
    assert AuthMiddleware._is_proxy_path("/sandboxes/s1/proxy/not-a-port/x") is False
    assert AuthMiddleware._is_proxy_path("/sandboxes/s1/proxy/8080x/") is False
