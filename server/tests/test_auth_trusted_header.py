# Copyright 2025 Alibaba Group Holding Ltd.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.

from fastapi import FastAPI, Request
from fastapi.testclient import TestClient

from opensandbox_server.config import (
    AUTH_MODE_API_KEY_AND_USER,
    AppConfig,
    AuthConfig,
    AuthzConfig,
    IngressConfig,
    RuntimeConfig,
    ServerConfig,
    TrustedHeaderConfig,
)
from opensandbox_server.middleware.auth import AuthMiddleware


def _app_dual_auth() -> AppConfig:
    return AppConfig(
        server=ServerConfig(api_key="api-secret"),
        auth=AuthConfig(
            mode=AUTH_MODE_API_KEY_AND_USER,
            trusted_header=TrustedHeaderConfig(
                user_header="X-OpenSandbox-User",
                team_header="X-OpenSandbox-Team",
                roles_header="X-OpenSandbox-Roles",
            ),
        ),
        authz=AuthzConfig(),
        runtime=RuntimeConfig(type="docker", execd_image="opensandbox/execd:latest"),
        ingress=IngressConfig(mode="direct"),
    )


def test_trusted_user_missing_identity_returns_401():
    app = FastAPI()
    app.add_middleware(AuthMiddleware, config=_app_dual_auth())

    @app.get("/secured")
    def secured():
        return {"ok": True}

    c = TestClient(app)
    r = c.get("/secured")
    assert r.status_code == 401
    assert r.json()["code"] == "MISSING_TRUSTED_IDENTITY"


def test_trusted_user_with_user_and_roles_succeeds():
    app = FastAPI()
    app.add_middleware(AuthMiddleware, config=_app_dual_auth())

    @app.get("/who")
    def who(request: Request):
        p = getattr(request.state, "principal", None)
        return {"subject": p.subject, "role": p.role if p else None}

    c = TestClient(app)
    r = c.get(
        "/who",
        headers={
            "X-OpenSandbox-User": "dev-user",
            "X-OpenSandbox-Roles": "read_only",
        },
    )
    assert r.status_code == 200
    assert r.json()["subject"] == "dev-user"
    assert r.json()["role"] == "read_only"


def test_trusted_user_with_only_user_header_uses_default_role():
    app = FastAPI()
    app.add_middleware(AuthMiddleware, config=_app_dual_auth())

    @app.get("/who")
    def who(request: Request):
        p = getattr(request.state, "principal", None)
        return {"subject": p.subject, "role": p.role if p else None}

    c = TestClient(app)
    r = c.get(
        "/who",
        headers={
            "X-OpenSandbox-User": "dev-user",
        },
    )
    assert r.status_code == 200
    assert r.json()["subject"] == "dev-user"
    assert r.json()["role"] == "read_only"


def test_valid_api_key_grants_service_admin():
    app = FastAPI()
    app.add_middleware(AuthMiddleware, config=_app_dual_auth())

    @app.get("/role")
    def role(request: Request):
        p = getattr(request.state, "principal", None)
        return {"admin": bool(p and p.is_service_admin)}

    c = TestClient(app)
    r = c.get("/role", headers={"OPEN-SANDBOX-API-KEY": "api-secret"})
    assert r.status_code == 200
    assert r.json() == {"admin": True}


def test_api_key_mismatch_still_401_with_valid_user_headers():
    app = FastAPI()
    app.add_middleware(AuthMiddleware, config=_app_dual_auth())

    @app.get("/x")
    def x():
        return {"n": 1}

    c = TestClient(app)
    r = c.get(
        "/x",
        headers={
            "OPEN-SANDBOX-API-KEY": "wrong",
            "X-OpenSandbox-User": "u",
            "X-OpenSandbox-Roles": "operator",
        },
    )
    assert r.status_code == 401
    assert r.json()["code"] == "INVALID_API_KEY"
