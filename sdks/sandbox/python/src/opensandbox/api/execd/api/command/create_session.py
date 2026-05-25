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
from typing import Any

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.create_session_request import CreateSessionRequest
from ...models.create_session_response import CreateSessionResponse
from ...models.error_response import ErrorResponse
from ...types import UNSET, Response, Unset


def _get_kwargs(
    *,
    body: CreateSessionRequest | Unset = UNSET,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/session",
    }

    if not isinstance(body, Unset):
        _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> CreateSessionResponse | ErrorResponse | None:
    if response.status_code == 200:
        response_200 = CreateSessionResponse.from_dict(response.json())

        return response_200

    if response.status_code == 400:
        response_400 = ErrorResponse.from_dict(response.json())

        return response_400

    if response.status_code == 500:
        response_500 = ErrorResponse.from_dict(response.json())

        return response_500

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[CreateSessionResponse | ErrorResponse]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: CreateSessionRequest | Unset = UNSET,
) -> Response[CreateSessionResponse | ErrorResponse]:
    """Create bash session (create_session)

     Creates a new bash session and returns a session ID for subsequent run_in_session requests.
    The session maintains shell state (e.g. working directory, environment) across multiple
    code executions. Request body is optional; an empty body uses default options (no cwd override).

    Args:
        body (CreateSessionRequest | Unset): Request to create a bash session (optional body;
            empty treated as defaults)

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[CreateSessionResponse | ErrorResponse]
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
    body: CreateSessionRequest | Unset = UNSET,
) -> CreateSessionResponse | ErrorResponse | None:
    """Create bash session (create_session)

     Creates a new bash session and returns a session ID for subsequent run_in_session requests.
    The session maintains shell state (e.g. working directory, environment) across multiple
    code executions. Request body is optional; an empty body uses default options (no cwd override).

    Args:
        body (CreateSessionRequest | Unset): Request to create a bash session (optional body;
            empty treated as defaults)

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        CreateSessionResponse | ErrorResponse
    """

    return sync_detailed(
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: CreateSessionRequest | Unset = UNSET,
) -> Response[CreateSessionResponse | ErrorResponse]:
    """Create bash session (create_session)

     Creates a new bash session and returns a session ID for subsequent run_in_session requests.
    The session maintains shell state (e.g. working directory, environment) across multiple
    code executions. Request body is optional; an empty body uses default options (no cwd override).

    Args:
        body (CreateSessionRequest | Unset): Request to create a bash session (optional body;
            empty treated as defaults)

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[CreateSessionResponse | ErrorResponse]
    """

    kwargs = _get_kwargs(
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    body: CreateSessionRequest | Unset = UNSET,
) -> CreateSessionResponse | ErrorResponse | None:
    """Create bash session (create_session)

     Creates a new bash session and returns a session ID for subsequent run_in_session requests.
    The session maintains shell state (e.g. working directory, environment) across multiple
    code executions. Request body is optional; an empty body uses default options (no cwd override).

    Args:
        body (CreateSessionRequest | Unset): Request to create a bash session (optional body;
            empty treated as defaults)

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        CreateSessionResponse | ErrorResponse
    """

    return (
        await asyncio_detailed(
            client=client,
            body=body,
        )
    ).parsed
