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

from fastapi import APIRouter, Depends, HTTPException, Response, status
from pydantic import BaseModel, ConfigDict, Field

from app.config import get_settings
from app.deps import get_session_payload
from app.session import admin_session, encode_session, tenant_session
from app.tenants import lookup_by_api_key

router = APIRouter(prefix="/auth", tags=["auth"])


class TenantLoginBody(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    api_key: str = Field(..., alias="apiKey")


class AdminLoginBody(BaseModel):
    model_config = ConfigDict(populate_by_name=True)

    admin_token: str = Field(..., alias="adminToken")


def _set_session_cookie(response: Response, token: str) -> None:
    settings = get_settings()
    response.set_cookie(
        key=settings.bff_session_cookie_name,
        value=token,
        httponly=True,
        secure=settings.bff_cookie_secure,
        samesite="lax",
        max_age=settings.bff_session_max_age_seconds,
        path="/",
    )


@router.post("/tenant")
async def login_tenant(body: TenantLoginBody, response: Response) -> dict:
    settings = get_settings()
    tenant = lookup_by_api_key(settings.tenants_toml_path, body.api_key)
    if tenant is None:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={"code": "INVALID_API_KEY", "message": "Invalid tenant API key"},
        )
    token = encode_session(settings, tenant_session(tenant.name, tenant.namespace))
    _set_session_cookie(response, token)
    return {"role": "tenant", "tenant": tenant.name, "namespace": tenant.namespace}


@router.post("/admin")
async def login_admin(body: AdminLoginBody, response: Response) -> dict:
    settings = get_settings()
    if body.admin_token != settings.bff_admin_token:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={"code": "INVALID_ADMIN_TOKEN", "message": "Invalid admin token"},
        )
    token = encode_session(settings, admin_session())
    _set_session_cookie(response, token)
    return {"role": "admin"}


@router.post("/logout")
async def logout(response: Response) -> dict:
    settings = get_settings()
    response.delete_cookie(settings.bff_session_cookie_name, path="/")
    return {"ok": True}


@router.get("/me")
async def me(payload: dict = Depends(get_session_payload)) -> dict:
    role = payload.get("role")
    if role == "tenant":
        return {
            "role": "tenant",
            "tenant": payload.get("tenant"),
            "namespace": payload.get("namespace"),
        }
    if role == "admin":
        return {"role": "admin"}
    raise HTTPException(status_code=status.HTTP_401_UNAUTHORIZED, detail={"code": "UNAUTHORIZED"})
