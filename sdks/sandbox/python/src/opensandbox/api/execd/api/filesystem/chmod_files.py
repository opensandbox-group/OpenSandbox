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

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.chmod_files_body import ChmodFilesBody
from ...models.error_response import ErrorResponse
from ...types import Response


def _get_kwargs(
    *,
    body: ChmodFilesBody,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/files/permissions",
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(*, client: AuthenticatedClient | Client, response: httpx.Response) -> Any | ErrorResponse | None:
    if response.status_code == 200:
        response_200 = cast(Any, None)
        return response_200

    if response.status_code == 400:
        try:
            response_400 = ErrorResponse.from_dict(response.json())
        except (ValueError, TypeError, KeyError):
            return None

        return response_400

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


def _build_response(*, client: AuthenticatedClient | Client, response: httpx.Response) -> Response[Any | ErrorResponse]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: ChmodFilesBody,
) -> Response[Any | ErrorResponse]:
    """Change file permissions

     Changes permissions (mode), owner, and group for one or multiple files.
    Accepts a map of file paths to permission settings including octal mode,
    owner username, and group name.

    Args:
        body (ChmodFilesBody):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | ErrorResponse]
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
    body: ChmodFilesBody,
) -> Any | ErrorResponse | None:
    """Change file permissions

     Changes permissions (mode), owner, and group for one or multiple files.
    Accepts a map of file paths to permission settings including octal mode,
    owner username, and group name.

    Args:
        body (ChmodFilesBody):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | ErrorResponse
    """

    return sync_detailed(
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: ChmodFilesBody,
) -> Response[Any | ErrorResponse]:
    """Change file permissions

     Changes permissions (mode), owner, and group for one or multiple files.
    Accepts a map of file paths to permission settings including octal mode,
    owner username, and group name.

    Args:
        body (ChmodFilesBody):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | ErrorResponse]
    """

    kwargs = _get_kwargs(
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    body: ChmodFilesBody,
) -> Any | ErrorResponse | None:
    """Change file permissions

     Changes permissions (mode), owner, and group for one or multiple files.
    Accepts a map of file paths to permission settings including octal mode,
    owner username, and group name.

    Args:
        body (ChmodFilesBody):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | ErrorResponse
    """

    return (
        await asyncio_detailed(
            client=client,
            body=body,
        )
    ).parsed
