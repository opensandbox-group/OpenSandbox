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
from ...types import UNSET, Response, Unset


def _get_kwargs(
    id: str,
    *,
    cursor: int | Unset = UNSET,
) -> dict[str, Any]:
    params: dict[str, Any] = {}

    params["cursor"] = cursor

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/command/{id}/logs".format(
            id=quote(str(id), safe=""),
        ),
        "params": params,
    }

    return _kwargs


def _parse_response(*, client: AuthenticatedClient | Client, response: httpx.Response) -> ErrorResponse | str | None:
    if response.status_code == 200:
        response_200 = response.text
        return response_200

    if response.status_code == 400:
        response_400 = ErrorResponse.from_dict(response.json())

        return response_400

    if response.status_code == 404:
        response_404 = ErrorResponse.from_dict(response.json())

        return response_404

    if response.status_code == 500:
        response_500 = ErrorResponse.from_dict(response.json())

        return response_500

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(*, client: AuthenticatedClient | Client, response: httpx.Response) -> Response[ErrorResponse | str]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    cursor: int | Unset = UNSET,
) -> Response[ErrorResponse | str]:
    """Get background command stdout/stderr (non-streamed)

     Returns stdout and stderr for a background (detached) command by command ID.
    Foreground commands should be consumed via SSE; this endpoint is intended for
    polling logs of background commands. Supports incremental reads similar to a file seek:
    pass the byte offset returned by the previous response (via `EXECD-COMMANDS-TAIL-CURSOR`)
    to fetch output after that position and receive the latest tail cursor for the next poll.
    When no cursor is provided, the full logs are returned.
    Response body is plain text so it can be rendered directly in browsers; the latest
    byte offset is provided via response header `EXECD-COMMANDS-TAIL-CURSOR` for subsequent
    incremental requests.

    Args:
        id (str):
        cursor (int | Unset):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | str]
    """

    kwargs = _get_kwargs(
        id=id,
        cursor=cursor,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    cursor: int | Unset = UNSET,
) -> ErrorResponse | str | None:
    """Get background command stdout/stderr (non-streamed)

     Returns stdout and stderr for a background (detached) command by command ID.
    Foreground commands should be consumed via SSE; this endpoint is intended for
    polling logs of background commands. Supports incremental reads similar to a file seek:
    pass the byte offset returned by the previous response (via `EXECD-COMMANDS-TAIL-CURSOR`)
    to fetch output after that position and receive the latest tail cursor for the next poll.
    When no cursor is provided, the full logs are returned.
    Response body is plain text so it can be rendered directly in browsers; the latest
    byte offset is provided via response header `EXECD-COMMANDS-TAIL-CURSOR` for subsequent
    incremental requests.

    Args:
        id (str):
        cursor (int | Unset):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | str
    """

    return sync_detailed(
        id=id,
        client=client,
        cursor=cursor,
    ).parsed


async def asyncio_detailed(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    cursor: int | Unset = UNSET,
) -> Response[ErrorResponse | str]:
    """Get background command stdout/stderr (non-streamed)

     Returns stdout and stderr for a background (detached) command by command ID.
    Foreground commands should be consumed via SSE; this endpoint is intended for
    polling logs of background commands. Supports incremental reads similar to a file seek:
    pass the byte offset returned by the previous response (via `EXECD-COMMANDS-TAIL-CURSOR`)
    to fetch output after that position and receive the latest tail cursor for the next poll.
    When no cursor is provided, the full logs are returned.
    Response body is plain text so it can be rendered directly in browsers; the latest
    byte offset is provided via response header `EXECD-COMMANDS-TAIL-CURSOR` for subsequent
    incremental requests.

    Args:
        id (str):
        cursor (int | Unset):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | str]
    """

    kwargs = _get_kwargs(
        id=id,
        cursor=cursor,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    id: str,
    *,
    client: AuthenticatedClient | Client,
    cursor: int | Unset = UNSET,
) -> ErrorResponse | str | None:
    """Get background command stdout/stderr (non-streamed)

     Returns stdout and stderr for a background (detached) command by command ID.
    Foreground commands should be consumed via SSE; this endpoint is intended for
    polling logs of background commands. Supports incremental reads similar to a file seek:
    pass the byte offset returned by the previous response (via `EXECD-COMMANDS-TAIL-CURSOR`)
    to fetch output after that position and receive the latest tail cursor for the next poll.
    When no cursor is provided, the full logs are returned.
    Response body is plain text so it can be rendered directly in browsers; the latest
    byte offset is provided via response header `EXECD-COMMANDS-TAIL-CURSOR` for subsequent
    incremental requests.

    Args:
        id (str):
        cursor (int | Unset):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | str
    """

    return (
        await asyncio_detailed(
            id=id,
            client=client,
            cursor=cursor,
        )
    ).parsed
