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
from io import BytesIO
from typing import Any

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error_response import ErrorResponse
from ...types import UNSET, File, Response, Unset


def _get_kwargs(
    *,
    path: str,
    offset: int | Unset = UNSET,
    limit: int | Unset = UNSET,
    range_: str | Unset = UNSET,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}
    if not isinstance(range_, Unset):
        headers["Range"] = range_

    params: dict[str, Any] = {}

    params["path"] = path

    params["offset"] = offset

    params["limit"] = limit

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/files/download",
        "params": params,
    }

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(*, client: AuthenticatedClient | Client, response: httpx.Response) -> ErrorResponse | File | None:
    if response.status_code == 200:
        response_200 = File(payload=BytesIO(response.content))

        return response_200

    if response.status_code == 206:
        response_206 = File(payload=BytesIO(response.content))

        return response_206

    if response.status_code == 400:
        response_400 = ErrorResponse.from_dict(response.json())

        return response_400

    if response.status_code == 404:
        response_404 = ErrorResponse.from_dict(response.json())

        return response_404

    if response.status_code == 416:
        response_416 = ErrorResponse.from_dict(response.json())

        return response_416

    if response.status_code == 500:
        response_500 = ErrorResponse.from_dict(response.json())

        return response_500

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[ErrorResponse | File]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
    path: str,
    offset: int | Unset = UNSET,
    limit: int | Unset = UNSET,
    range_: str | Unset = UNSET,
) -> Response[ErrorResponse | File]:
    """Download file from sandbox

     Downloads a file from the specified path within the sandbox. Supports HTTP
    range requests for resumable downloads and partial content retrieval.
    Returns file as octet-stream with appropriate headers.

    When offset/limit query parameters are provided, the endpoint performs
    line-based reading and returns text/plain content instead. Line-based
    parameters are mutually exclusive with the Range header.

    Args:
        path (str):
        offset (int | Unset):
        limit (int | Unset):
        range_ (str | Unset):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | File]
    """

    kwargs = _get_kwargs(
        path=path,
        offset=offset,
        limit=limit,
        range_=range_,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    *,
    client: AuthenticatedClient | Client,
    path: str,
    offset: int | Unset = UNSET,
    limit: int | Unset = UNSET,
    range_: str | Unset = UNSET,
) -> ErrorResponse | File | None:
    """Download file from sandbox

     Downloads a file from the specified path within the sandbox. Supports HTTP
    range requests for resumable downloads and partial content retrieval.
    Returns file as octet-stream with appropriate headers.

    When offset/limit query parameters are provided, the endpoint performs
    line-based reading and returns text/plain content instead. Line-based
    parameters are mutually exclusive with the Range header.

    Args:
        path (str):
        offset (int | Unset):
        limit (int | Unset):
        range_ (str | Unset):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | File
    """

    return sync_detailed(
        client=client,
        path=path,
        offset=offset,
        limit=limit,
        range_=range_,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    path: str,
    offset: int | Unset = UNSET,
    limit: int | Unset = UNSET,
    range_: str | Unset = UNSET,
) -> Response[ErrorResponse | File]:
    """Download file from sandbox

     Downloads a file from the specified path within the sandbox. Supports HTTP
    range requests for resumable downloads and partial content retrieval.
    Returns file as octet-stream with appropriate headers.

    When offset/limit query parameters are provided, the endpoint performs
    line-based reading and returns text/plain content instead. Line-based
    parameters are mutually exclusive with the Range header.

    Args:
        path (str):
        offset (int | Unset):
        limit (int | Unset):
        range_ (str | Unset):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | File]
    """

    kwargs = _get_kwargs(
        path=path,
        offset=offset,
        limit=limit,
        range_=range_,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    path: str,
    offset: int | Unset = UNSET,
    limit: int | Unset = UNSET,
    range_: str | Unset = UNSET,
) -> ErrorResponse | File | None:
    """Download file from sandbox

     Downloads a file from the specified path within the sandbox. Supports HTTP
    range requests for resumable downloads and partial content retrieval.
    Returns file as octet-stream with appropriate headers.

    When offset/limit query parameters are provided, the endpoint performs
    line-based reading and returns text/plain content instead. Line-based
    parameters are mutually exclusive with the Range header.

    Args:
        path (str):
        offset (int | Unset):
        limit (int | Unset):
        range_ (str | Unset):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | File
    """

    return (
        await asyncio_detailed(
            client=client,
            path=path,
            offset=offset,
            limit=limit,
            range_=range_,
        )
    ).parsed
