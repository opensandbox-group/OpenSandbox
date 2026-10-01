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
    depth: int | Unset = 1,
) -> dict[str, Any]:
    params: dict[str, Any] = {}

    params["path"] = path

    params["depth"] = depth

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/v1/filesystem/{uid}/{gid}/directories/list".format(
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
    depth: int | Unset = 1,
) -> Response[Any | ErrorResponse | list[FileInfo]]:
    """List directory contents

     Lists entries under a directory with optional depth control. By default,
    only immediate children are returned (`depth=1`). Set `depth` to a larger
    value to include descendants up to that many levels below `path`. The
    root directory itself is not included in the response.

    Symbolic links are reported with `type=symlink` and are not traversed:
    the listing never descends into a link target, even when `depth` would
    otherwise allow it. For the same reason, when `path` itself resolves to
    a symbolic link the request is rejected with `400`; callers must pass
    the real directory path they want listed.

    Entries are returned in lexical order by entry name within each
    directory. Descendants reported via `depth>1` follow their parent in
    the same lexical order, so a depth-2 listing yields stable, predictable
    output for file-browser style clients.

    Args:
        uid (int):
        gid (int):
        path (str):
        depth (int | Unset):  Default: 1.

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
        depth=depth,
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
    depth: int | Unset = 1,
) -> Any | ErrorResponse | list[FileInfo] | None:
    """List directory contents

     Lists entries under a directory with optional depth control. By default,
    only immediate children are returned (`depth=1`). Set `depth` to a larger
    value to include descendants up to that many levels below `path`. The
    root directory itself is not included in the response.

    Symbolic links are reported with `type=symlink` and are not traversed:
    the listing never descends into a link target, even when `depth` would
    otherwise allow it. For the same reason, when `path` itself resolves to
    a symbolic link the request is rejected with `400`; callers must pass
    the real directory path they want listed.

    Entries are returned in lexical order by entry name within each
    directory. Descendants reported via `depth>1` follow their parent in
    the same lexical order, so a depth-2 listing yields stable, predictable
    output for file-browser style clients.

    Args:
        uid (int):
        gid (int):
        path (str):
        depth (int | Unset):  Default: 1.

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
        depth=depth,
    ).parsed


async def asyncio_detailed(
    uid: int,
    gid: int,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    depth: int | Unset = 1,
) -> Response[Any | ErrorResponse | list[FileInfo]]:
    """List directory contents

     Lists entries under a directory with optional depth control. By default,
    only immediate children are returned (`depth=1`). Set `depth` to a larger
    value to include descendants up to that many levels below `path`. The
    root directory itself is not included in the response.

    Symbolic links are reported with `type=symlink` and are not traversed:
    the listing never descends into a link target, even when `depth` would
    otherwise allow it. For the same reason, when `path` itself resolves to
    a symbolic link the request is rejected with `400`; callers must pass
    the real directory path they want listed.

    Entries are returned in lexical order by entry name within each
    directory. Descendants reported via `depth>1` follow their parent in
    the same lexical order, so a depth-2 listing yields stable, predictable
    output for file-browser style clients.

    Args:
        uid (int):
        gid (int):
        path (str):
        depth (int | Unset):  Default: 1.

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
        depth=depth,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    uid: int,
    gid: int,
    *,
    client: AuthenticatedClient | Client,
    path: str,
    depth: int | Unset = 1,
) -> Any | ErrorResponse | list[FileInfo] | None:
    """List directory contents

     Lists entries under a directory with optional depth control. By default,
    only immediate children are returned (`depth=1`). Set `depth` to a larger
    value to include descendants up to that many levels below `path`. The
    root directory itself is not included in the response.

    Symbolic links are reported with `type=symlink` and are not traversed:
    the listing never descends into a link target, even when `depth` would
    otherwise allow it. For the same reason, when `path` itself resolves to
    a symbolic link the request is rejected with `400`; callers must pass
    the real directory path they want listed.

    Entries are returned in lexical order by entry name within each
    directory. Descendants reported via `depth>1` follow their parent in
    the same lexical order, so a depth-2 listing yields stable, predictable
    output for file-browser style clients.

    Args:
        uid (int):
        gid (int):
        path (str):
        depth (int | Unset):  Default: 1.

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
            depth=depth,
        )
    ).parsed
