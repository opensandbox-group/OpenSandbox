#
# Copyright 2026 Alibaba Group Holding Ltd.
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
from typing import Any, cast

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.create_pty_operation_request import CreatePTYOperationRequest
from ...models.error_response import ErrorResponse
from ...models.execution_operation import ExecutionOperation
from ...types import Response


def _get_kwargs(
    *,
    body: CreatePTYOperationRequest,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/pty/operations",
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Any | ErrorResponse | ExecutionOperation | None:
    if response.status_code == 200:
        response_200 = ExecutionOperation.from_dict(response.json())

        return response_200

    if response.status_code == 202:
        response_202 = ExecutionOperation.from_dict(response.json())

        return response_202

    if response.status_code == 400:
        response_400 = ErrorResponse.from_dict(response.json())

        return response_400

    if response.status_code == 401:
        response_401 = ErrorResponse.from_dict(response.json())

        return response_401

    if response.status_code == 409:
        response_409 = ErrorResponse.from_dict(response.json())

        return response_409

    if response.status_code == 410:
        response_410 = ErrorResponse.from_dict(response.json())

        return response_410

    if response.status_code == 501:
        response_501 = cast(Any, None)
        return response_501

    if response.status_code == 503:
        response_503 = ErrorResponse.from_dict(response.json())

        return response_503

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[Any | ErrorResponse | ExecutionOperation]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: CreatePTYOperationRequest,
) -> Response[Any | ErrorResponse | ExecutionOperation]:
    """Create or recover a caller-bound PTY session

     Creates or recovers the same dormant PTY session using a persisted operation_id. The first WebSocket
    connection launches the process; a caller-bound session permits one launch attempt and reuses
    existing replay/takeover/terminal frames. This POST does not start a shell. Unknown fields are
    rejected. Body limit is 1 MiB.

    Args:
        body (CreatePTYOperationRequest):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | ErrorResponse | ExecutionOperation]
    """

    kwargs = _get_kwargs(
        body=body,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    *,
    client: AuthenticatedClient | Client,
    body: CreatePTYOperationRequest,
) -> Any | ErrorResponse | ExecutionOperation | None:
    """Create or recover a caller-bound PTY session

     Creates or recovers the same dormant PTY session using a persisted operation_id. The first WebSocket
    connection launches the process; a caller-bound session permits one launch attempt and reuses
    existing replay/takeover/terminal frames. This POST does not start a shell. Unknown fields are
    rejected. Body limit is 1 MiB.

    Args:
        body (CreatePTYOperationRequest):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | ErrorResponse | ExecutionOperation
    """

    return sync_detailed(
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: CreatePTYOperationRequest,
) -> Response[Any | ErrorResponse | ExecutionOperation]:
    """Create or recover a caller-bound PTY session

     Creates or recovers the same dormant PTY session using a persisted operation_id. The first WebSocket
    connection launches the process; a caller-bound session permits one launch attempt and reuses
    existing replay/takeover/terminal frames. This POST does not start a shell. Unknown fields are
    rejected. Body limit is 1 MiB.

    Args:
        body (CreatePTYOperationRequest):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | ErrorResponse | ExecutionOperation]
    """

    kwargs = _get_kwargs(
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    body: CreatePTYOperationRequest,
) -> Any | ErrorResponse | ExecutionOperation | None:
    """Create or recover a caller-bound PTY session

     Creates or recovers the same dormant PTY session using a persisted operation_id. The first WebSocket
    connection launches the process; a caller-bound session permits one launch attempt and reuses
    existing replay/takeover/terminal frames. This POST does not start a shell. Unknown fields are
    rejected. Body limit is 1 MiB.

    Args:
        body (CreatePTYOperationRequest):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | ErrorResponse | ExecutionOperation
    """

    return (
        await asyncio_detailed(
            client=client,
            body=body,
        )
    ).parsed
