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

from fastapi import HTTPException, status

from app.config import get_settings
from app.tenants import get_tenant_by_name


def tenant_api_key(payload: dict) -> str:
    settings = get_settings()
    record = get_tenant_by_name(settings.tenants_toml_path, payload["tenant"])
    if record is None:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={"code": "TENANT_REMOVED", "message": "Tenant no longer configured"},
        )
    return record.api_key


def tenant_namespace_for_name(tenant_name: str) -> str:
    settings = get_settings()
    record = get_tenant_by_name(settings.tenants_toml_path, tenant_name)
    if record is None:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": "UNKNOWN_TENANT",
                "message": f"Tenant {tenant_name!r} is not configured",
            },
        )
    return record.namespace


def tenant_namespace_for_session(payload: dict) -> str:
    settings = get_settings()
    record = get_tenant_by_name(settings.tenants_toml_path, payload["tenant"])
    if record is None:
        raise HTTPException(
            status_code=status.HTTP_401_UNAUTHORIZED,
            detail={"code": "TENANT_REMOVED", "message": "Tenant no longer configured"},
        )
    return record.namespace


def api_key_for_tenant_name(tenant_name: str) -> str:
    settings = get_settings()
    record = get_tenant_by_name(settings.tenants_toml_path, tenant_name)
    if record is None:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": "UNKNOWN_TENANT",
                "message": f"Tenant {tenant_name!r} is not configured",
            },
        )
    return record.api_key
