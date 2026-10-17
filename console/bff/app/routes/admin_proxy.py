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

from typing import Any

from fastapi import Request

from app.config import get_settings
from app.lifecycle import LifecycleClient
from app.routes.proxy_utils import passthrough_json
from app.routes.session_keys import api_key_for_tenant_name
from app.runtime import attach_runtime_summary


async def admin_lifecycle_request(
    tenant: str,
    method: str,
    path: str,
    *,
    request: Request | None = None,
    json_body: dict[str, Any] | None = None,
):
    api_key = api_key_for_tenant_name(tenant)
    params = dict(request.query_params) if request else None
    if params and "tenant" in params:
        params = {k: v for k, v in params.items() if k != "tenant"}
    client = LifecycleClient(get_settings())
    resp = await client.request(
        api_key,
        method,
        path,
        params=params,
        json_body=json_body,
    )
    return await passthrough_json(resp)


async def admin_get_sandbox(tenant: str, sandbox_id: str) -> Any:
    data = await admin_lifecycle_request(tenant, "GET", f"/sandboxes/{sandbox_id}")
    if isinstance(data, dict):
        enriched = attach_runtime_summary(data)
        enriched["tenant"] = tenant
        return enriched
    return data
