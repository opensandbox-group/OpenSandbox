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
from uuid import UUID

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error_response import ErrorResponse
from ...models.file_info import FileInfo
from ...types import UNSET, Response, Unset


def _get_kwargs(
    session_id: UUID,
    *,
    path: str,
    pattern: str | Unset = "**",
) -> dict[str, Any]:
    params: dict[str, Any] = {}

    params["path"] = path

    params["pattern"] = pattern

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/v1/isolated/session/{session_id}/files/search".format(
            session_id=quote(str(session_id), safe=""),
        ),
        "params": params,
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> ErrorResponse | list[FileInfo] | None:
    if response.status_code == 200:
        response_200 = []
        _response_200 = response.json()
        for response_200_item_data in _response_200:
            response_200_item = FileInfo.from_dict(response_200_item_data)

            response_200.append(response_200_item)

        return response_200

    if response.status_code == 400:
        response_400 = ErrorResponse.from_dict(response.json())

        return response_400

    if response.status_code == 503:
        response_503 = ErrorResponse.from_dict(response.json())

        return response_503

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[ErrorResponse | list[FileInfo]]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    session_id: UUID,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    pattern: str | Unset = "**",
) -> Response[ErrorResponse | list[FileInfo]]:
    """Search files

    Args:
        session_id (UUID):
        path (str):
        pattern (str | Unset):  Default: '**'.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | list[FileInfo]]
    """

    kwargs = _get_kwargs(
        session_id=session_id,
        path=path,
        pattern=pattern,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    session_id: UUID,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    pattern: str | Unset = "**",
) -> ErrorResponse | list[FileInfo] | None:
    """Search files

    Args:
        session_id (UUID):
        path (str):
        pattern (str | Unset):  Default: '**'.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | list[FileInfo]
    """

    return sync_detailed(
        session_id=session_id,
        client=client,
        path=path,
        pattern=pattern,
    ).parsed


async def asyncio_detailed(
    session_id: UUID,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    pattern: str | Unset = "**",
) -> Response[ErrorResponse | list[FileInfo]]:
    """Search files

    Args:
        session_id (UUID):
        path (str):
        pattern (str | Unset):  Default: '**'.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | list[FileInfo]]
    """

    kwargs = _get_kwargs(
        session_id=session_id,
        path=path,
        pattern=pattern,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    session_id: UUID,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    pattern: str | Unset = "**",
) -> ErrorResponse | list[FileInfo] | None:
    """Search files

    Args:
        session_id (UUID):
        path (str):
        pattern (str | Unset):  Default: '**'.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | list[FileInfo]
    """

    return (
        await asyncio_detailed(
            session_id=session_id,
            client=client,
            path=path,
            pattern=pattern,
        )
    ).parsed
