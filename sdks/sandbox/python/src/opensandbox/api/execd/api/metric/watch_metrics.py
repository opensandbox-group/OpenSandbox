#
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
#

from http import HTTPStatus
from typing import Any

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error_response import ErrorResponse
from ...models.metrics import Metrics
from ...types import Response


def _get_kwargs() -> dict[str, Any]:
    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/metrics/watch",
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> ErrorResponse | Metrics | None:
    if response.status_code == 200:
        response_200 = Metrics.from_dict(response.text)

        return response_200

    if response.status_code == 500:
        try:
            response_500 = ErrorResponse.from_dict(response.json())
        except (ValueError, TypeError, KeyError):
            return None

        return response_500

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[ErrorResponse | Metrics]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
) -> Response[ErrorResponse | Metrics]:
    """Watch system metrics in real-time

     Streams system resource metrics in real-time using Server-Sent Events (SSE).
    Updates are sent every second, providing continuous monitoring of CPU usage,
    memory usage, and other system metrics. The connection remains open until
    the client disconnects.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | Metrics]
    """

    kwargs = _get_kwargs()

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    *,
    client: AuthenticatedClient | Client,
) -> ErrorResponse | Metrics | None:
    """Watch system metrics in real-time

     Streams system resource metrics in real-time using Server-Sent Events (SSE).
    Updates are sent every second, providing continuous monitoring of CPU usage,
    memory usage, and other system metrics. The connection remains open until
    the client disconnects.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | Metrics
    """

    return sync_detailed(
        client=client,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
) -> Response[ErrorResponse | Metrics]:
    """Watch system metrics in real-time

     Streams system resource metrics in real-time using Server-Sent Events (SSE).
    Updates are sent every second, providing continuous monitoring of CPU usage,
    memory usage, and other system metrics. The connection remains open until
    the client disconnects.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | Metrics]
    """

    kwargs = _get_kwargs()

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
) -> ErrorResponse | Metrics | None:
    """Watch system metrics in real-time

     Streams system resource metrics in real-time using Server-Sent Events (SSE).
    Updates are sent every second, providing continuous monitoring of CPU usage,
    memory usage, and other system metrics. The connection remains open until
    the client disconnects.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | Metrics
    """

    return (
        await asyncio_detailed(
            client=client,
        )
    ).parsed
