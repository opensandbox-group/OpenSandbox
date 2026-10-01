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
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error_response import ErrorResponse
from ...models.file_info import FileInfo
from ...types import UNSET, Response, Unset


def _get_kwargs(
    uid: int,
    gid: int,
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
        "url": "/v1/filesystem/{uid}/{gid}/files/search".format(
            uid=quote(str(uid), safe=""),
            gid=quote(str(gid), safe=""),
        ),
        "params": params,
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Any | ErrorResponse | list[FileInfo] | None:
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

    if response.status_code == 404:
        response_404 = ErrorResponse.from_dict(response.json())

        return response_404

    if response.status_code == 500:
        response_500 = ErrorResponse.from_dict(response.json())

        return response_500

    if response.status_code == 501:
        response_501 = cast(Any, None)
        return response_501

    if response.status_code == 503:
        response_503 = cast(Any, None)
        return response_503

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[Any | ErrorResponse | list[FileInfo]]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    uid: int,
    gid: int,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    pattern: str | Unset = "**",
) -> Response[Any | ErrorResponse | list[FileInfo]]:
    """Search for files

     Searches for files matching a glob pattern within a specified directory and
    its subdirectories. Returns file metadata including path, permissions, owner,
    and group. Supports glob patterns like **, *.txt, etc. Default pattern is ** (all files).

    Args:
        uid (int):
        gid (int):
        path (str):
        pattern (str | Unset):  Default: '**'.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | ErrorResponse | list[FileInfo]]
    """

    kwargs = _get_kwargs(
        uid=uid,
        gid=gid,
        path=path,
        pattern=pattern,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    uid: int,
    gid: int,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    pattern: str | Unset = "**",
) -> Any | ErrorResponse | list[FileInfo] | None:
    """Search for files

     Searches for files matching a glob pattern within a specified directory and
    its subdirectories. Returns file metadata including path, permissions, owner,
    and group. Supports glob patterns like **, *.txt, etc. Default pattern is ** (all files).

    Args:
        uid (int):
        gid (int):
        path (str):
        pattern (str | Unset):  Default: '**'.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | ErrorResponse | list[FileInfo]
    """

    return sync_detailed(
        uid=uid,
        gid=gid,
        client=client,
        path=path,
        pattern=pattern,
    ).parsed


async def asyncio_detailed(
    uid: int,
    gid: int,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    pattern: str | Unset = "**",
) -> Response[Any | ErrorResponse | list[FileInfo]]:
    """Search for files

     Searches for files matching a glob pattern within a specified directory and
    its subdirectories. Returns file metadata including path, permissions, owner,
    and group. Supports glob patterns like **, *.txt, etc. Default pattern is ** (all files).

    Args:
        uid (int):
        gid (int):
        path (str):
        pattern (str | Unset):  Default: '**'.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | ErrorResponse | list[FileInfo]]
    """

    kwargs = _get_kwargs(
        uid=uid,
        gid=gid,
        path=path,
        pattern=pattern,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    uid: int,
    gid: int,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    pattern: str | Unset = "**",
) -> Any | ErrorResponse | list[FileInfo] | None:
    """Search for files

     Searches for files matching a glob pattern within a specified directory and
    its subdirectories. Returns file metadata including path, permissions, owner,
    and group. Supports glob patterns like **, *.txt, etc. Default pattern is ** (all files).

    Args:
        uid (int):
        gid (int):
        path (str):
        pattern (str | Unset):  Default: '**'.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | ErrorResponse | list[FileInfo]
    """

    return (
        await asyncio_detailed(
            uid=uid,
            gid=gid,
            client=client,
            path=path,
            pattern=pattern,
        )
    ).parsed
