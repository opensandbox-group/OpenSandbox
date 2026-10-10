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

import httpx
from fastapi import HTTPException, status
from fastapi.responses import JSONResponse, Response


def upstream_error_response(resp: httpx.Response) -> JSONResponse:
    try:
        body = resp.json()
    except Exception:
        body = {"code": "UPSTREAM_ERROR", "message": resp.text or resp.reason_phrase}
    if isinstance(body, dict) and "code" not in body and "message" not in body:
        body = {"code": "UPSTREAM_ERROR", "message": str(body)}
    return JSONResponse(status_code=resp.status_code, content=body)


async def passthrough_json(resp: httpx.Response) -> Any:
    if resp.status_code >= 400:
        return upstream_error_response(resp)
    if resp.status_code in (202, 204) and not resp.content:
        return Response(status_code=resp.status_code)
    return resp.json()
