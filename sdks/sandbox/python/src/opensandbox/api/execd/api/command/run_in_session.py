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
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error_response import ErrorResponse
from ...models.run_in_session_request import RunInSessionRequest
from ...models.server_stream_event import ServerStreamEvent
from ...types import Response


def _get_kwargs(
    session_id: str,
    *,
    body: RunInSessionRequest,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/session/{session_id}/run".format(
            session_id=quote(str(session_id), safe=""),
        ),
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> ErrorResponse | ServerStreamEvent | None:
    if response.status_code == 200:
        response_200 = ServerStreamEvent.from_dict(response.text)

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
) -> Response[ErrorResponse | ServerStreamEvent]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    session_id: str,
    *,
    client: AuthenticatedClient | Client,
    body: RunInSessionRequest,
) -> Response[ErrorResponse | ServerStreamEvent]:
    """Run command in bash session (run_in_session)

     Executes a shell command in an existing bash session and streams the output in real-time via SSE
    (Server-Sent Events). The session must have been created by create_session. Supports
    optional working directory override and timeout (milliseconds).

    Args:
        session_id (str):
        body (RunInSessionRequest): Request to run a command in an existing bash session

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | ServerStreamEvent]
    """

    kwargs = _get_kwargs(
        session_id=session_id,
        body=body,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    session_id: str,
    *,
    client: AuthenticatedClient | Client,
    body: RunInSessionRequest,
) -> ErrorResponse | ServerStreamEvent | None:
    """Run command in bash session (run_in_session)

     Executes a shell command in an existing bash session and streams the output in real-time via SSE
    (Server-Sent Events). The session must have been created by create_session. Supports
    optional working directory override and timeout (milliseconds).

    Args:
        session_id (str):
        body (RunInSessionRequest): Request to run a command in an existing bash session

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | ServerStreamEvent
    """

    return sync_detailed(
        session_id=session_id,
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    session_id: str,
    *,
    client: AuthenticatedClient | Client,
    body: RunInSessionRequest,
) -> Response[ErrorResponse | ServerStreamEvent]:
    """Run command in bash session (run_in_session)

     Executes a shell command in an existing bash session and streams the output in real-time via SSE
    (Server-Sent Events). The session must have been created by create_session. Supports
    optional working directory override and timeout (milliseconds).

    Args:
        session_id (str):
        body (RunInSessionRequest): Request to run a command in an existing bash session

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | ServerStreamEvent]
    """

    kwargs = _get_kwargs(
        session_id=session_id,
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    session_id: str,
    *,
    client: AuthenticatedClient | Client,
    body: RunInSessionRequest,
) -> ErrorResponse | ServerStreamEvent | None:
    """Run command in bash session (run_in_session)

     Executes a shell command in an existing bash session and streams the output in real-time via SSE
    (Server-Sent Events). The session must have been created by create_session. Supports
    optional working directory override and timeout (milliseconds).

    Args:
        session_id (str):
        body (RunInSessionRequest): Request to run a command in an existing bash session

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | ServerStreamEvent
    """

    return (
        await asyncio_detailed(
            session_id=session_id,
            client=client,
            body=body,
        )
    ).parsed
