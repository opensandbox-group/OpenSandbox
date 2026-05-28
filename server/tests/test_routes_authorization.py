# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.

from datetime import datetime, timedelta, timezone

from fastapi import FastAPI
from fastapi.testclient import TestClient

from opensandbox_server.api import lifecycle
from opensandbox_server.api.schema import (
    CreateSandboxResponse,
    Endpoint,
    ImageSpec,
    ListSandboxesResponse,
    PaginationInfo,
    RenewSandboxExpirationResponse,
    Sandbox,
    SandboxStatus,
)
from opensandbox_server.config import (
    AUTH_MODE_API_KEY_AND_USER,
    AppConfig,
    AuthConfig,
    AuthzConfig,
    IngressConfig,
    RuntimeConfig,
    ServerConfig,
)
from opensandbox_server.middleware.auth import AuthMiddleware


def _cfg() -> AppConfig:
    return AppConfig(
        server=ServerConfig(api_key="api-secret"),
        auth=AuthConfig(mode=AUTH_MODE_API_KEY_AND_USER),
        authz=AuthzConfig(default_role="read_only"),
        runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:latest"),
        ingress=IngressConfig(mode="direct"),
    )


def _sandbox(owner: str) -> Sandbox:
    now = datetime.now(timezone.utc)
    return Sandbox(
        id="sbx-1",
        image=ImageSpec(uri="python:3.11"),
        status=SandboxStatus(state="Running"),
        metadata={"access.owner": owner},
        entrypoint=["python", "-V"],
        expiresAt=now + timedelta(hours=1),
        createdAt=now,
    )


def _build_app(monkeypatch, service_obj) -> TestClient:
    cfg = _cfg()
    app = FastAPI()
    app.add_middleware(AuthMiddleware, config=cfg)
    app.include_router(lifecycle.router, prefix="/v1")
    monkeypatch.setattr(lifecycle, "sandbox_service", service_obj)
    monkeypatch.setattr(lifecycle, "get_config", lambda: cfg)
    return TestClient(app)


def _user_headers(role: str) -> dict[str, str]:
    return {
        "X-OpenSandbox-User": "alice",
        "X-OpenSandbox-Roles": role,
    }


def test_read_only_can_list_get_and_endpoint_but_cannot_mutate(monkeypatch) -> None:
    owner = "alice"

    class StubService:
        @staticmethod
        def list_sandboxes(_request) -> ListSandboxesResponse:
            return ListSandboxesResponse(
                items=[_sandbox(owner)],
                pagination=PaginationInfo(
                    page=1,
                    pageSize=20,
                    totalItems=1,
                    totalPages=1,
                    hasNextPage=False,
                ),
            )

        @staticmethod
        def get_sandbox(_sandbox_id: str) -> Sandbox:
            return _sandbox(owner)

        @staticmethod
        def get_endpoint(_sandbox_id: str, _port: int, resolve_internal: bool = False, expires=None) -> Endpoint:
            return Endpoint(endpoint="127.0.0.1:18080")

    c = _build_app(monkeypatch, StubService())

    r_list = c.get("/v1/sandboxes", headers=_user_headers("read_only"))
    assert r_list.status_code == 200
    r_get = c.get("/v1/sandboxes/sbx-1", headers=_user_headers("read_only"))
    assert r_get.status_code == 200
    r_ep = c.get("/v1/sandboxes/sbx-1/endpoints/8080", headers=_user_headers("read_only"))
    assert r_ep.status_code == 200

    r_create = c.post(
        "/v1/sandboxes",
        headers=_user_headers("read_only"),
        json={
            "image": {"uri": "python:3.11"},
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
            "entrypoint": ["python", "-V"],
        },
    )
    assert r_create.status_code == 403
    payload = r_create.json()
    code = payload.get("code")
    if code is None and isinstance(payload.get("detail"), dict):
        code = payload["detail"].get("code")
    assert code == "INSUFFICIENT_ROLE"


def test_operator_can_mutate(monkeypatch) -> None:
    owner = "alice"

    class StubService:
        @staticmethod
        def get_sandbox(_sandbox_id: str) -> Sandbox:
            return _sandbox(owner)

        @staticmethod
        async def create_sandbox(_request) -> CreateSandboxResponse:
            now = datetime.now(timezone.utc)
            return CreateSandboxResponse(
                id="sbx-2",
                status=SandboxStatus(state="Pending"),
                metadata={"access.owner": owner},
                expiresAt=now + timedelta(hours=1),
                createdAt=now,
                entrypoint=["python", "-V"],
            )

        @staticmethod
        def delete_sandbox(_sandbox_id: str) -> None:
            return None

        @staticmethod
        def renew_expiration(_sandbox_id: str, _request) -> RenewSandboxExpirationResponse:
            return RenewSandboxExpirationResponse(expiresAt=datetime.now(timezone.utc) + timedelta(hours=2))

        @staticmethod
        def pause_sandbox(_sandbox_id: str) -> None:
            return None

        @staticmethod
        def resume_sandbox(_sandbox_id: str) -> None:
            return None

    c = _build_app(monkeypatch, StubService())
    h = _user_headers("operator")

    r_create = c.post(
        "/v1/sandboxes",
        headers=h,
        json={
            "image": {"uri": "python:3.11"},
            "timeout": 3600,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
            "entrypoint": ["python", "-V"],
        },
    )
    assert r_create.status_code == 202

    assert c.post("/v1/sandboxes/sbx-1/renew-expiration", headers=h, json={"expiresAt": "2030-01-01T00:00:00Z"}).status_code == 200
    assert c.post("/v1/sandboxes/sbx-1/pause", headers=h).status_code == 202
    assert c.post("/v1/sandboxes/sbx-1/resume", headers=h).status_code == 202
    assert c.delete("/v1/sandboxes/sbx-1", headers=h).status_code == 204
