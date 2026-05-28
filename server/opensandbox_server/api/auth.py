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
Auth introspection endpoint — returns the caller's resolved role so the
developer console can derive per-user UI permissions at runtime.
"""

from __future__ import annotations

from fastapi import APIRouter, Request
from pydantic import BaseModel

from opensandbox_server.api.lifecycle_helpers import get_principal

router = APIRouter(tags=["Auth"])


class WhoAmIResponse(BaseModel):
    role: str
    subject: str


@router.get(
    "/auth/whoami",
    response_model=WhoAmIResponse,
    responses={
        200: {"description": "Caller identity and effective role"},
        401: {"description": "Authentication credentials are missing or invalid"},
    },
)
async def whoami(http_request: Request) -> WhoAmIResponse:
    """
    Return the authenticated caller's effective role and subject.

    Used by the developer console to derive role-aware UI at runtime instead of
    relying on the build-time VITE_UI_ROLE environment variable.
    """
    principal = get_principal(http_request)
    if principal is None:
        return WhoAmIResponse(role="service_admin", subject="anonymous")
    return WhoAmIResponse(role=principal.role, subject=principal.subject)
