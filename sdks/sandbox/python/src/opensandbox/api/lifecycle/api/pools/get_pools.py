#
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
#

from http import HTTPStatus
from typing import Any

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error_response import ErrorResponse
from ...models.list_pools_response import ApiListPoolsResponse
from ...types import Response


def _get_kwargs() -> dict[str, Any]:
    return {"method": "get", "url": "/pools"}


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> ApiListPoolsResponse | ErrorResponse | None:
    if response.status_code == 200:
        return ApiListPoolsResponse.from_dict(response.json())
    if response.status_code in (401, 500, 501):
        return ErrorResponse.from_dict(response.json())
    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[ApiListPoolsResponse | ErrorResponse]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *, client: AuthenticatedClient | Client
) -> Response[ApiListPoolsResponse | ErrorResponse]:
    """List all pre-warmed resource pools."""
    response = client.get_httpx_client().request(**_get_kwargs())
    return _build_response(client=client, response=response)


async def asyncio_detailed(
    *, client: AuthenticatedClient | Client
) -> Response[ApiListPoolsResponse | ErrorResponse]:
    """List all pre-warmed resource pools."""
    response = await client.get_async_httpx_client().request(**_get_kwargs())
    return _build_response(client=client, response=response)
