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
from ...models.list_sandboxes_response import ListSandboxesResponse
from ...types import UNSET, Response, Unset


def _get_kwargs(
    *,
    state: list[str] | Unset = UNSET,
    metadata: str | Unset = UNSET,
    page: int | Unset = 1,
    page_size: int | Unset = 20,
) -> dict[str, Any]:
    params: dict[str, Any] = {}

    json_state: list[str] | Unset = UNSET
    if not isinstance(state, Unset):
        json_state = state

    params["state"] = json_state

    params["metadata"] = metadata

    params["page"] = page

    params["pageSize"] = page_size

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/sandboxes",
        "params": params,
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> ErrorResponse | ListSandboxesResponse | None:
    if response.status_code == 200:
        response_200 = ListSandboxesResponse.from_dict(response.json())

        return response_200

    if response.status_code == 400:
        try:
            response_400 = ErrorResponse.from_dict(response.json())
        except (ValueError, TypeError, KeyError):
            return None

        return response_400

    if response.status_code == 401:
        try:
            response_401 = ErrorResponse.from_dict(response.json())
        except (ValueError, TypeError, KeyError):
            return None

        return response_401

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
) -> Response[ErrorResponse | ListSandboxesResponse]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
    state: list[str] | Unset = UNSET,
    metadata: str | Unset = UNSET,
    page: int | Unset = 1,
    page_size: int | Unset = 20,
) -> Response[ErrorResponse | ListSandboxesResponse]:
    """List sandboxes

     List all sandboxes with optional filtering and pagination using query parameters.
    All filter conditions use AND logic. Multiple `state` parameters use OR logic within states.

    Args:
        state (list[str] | Unset):
        metadata (str | Unset):
        page (int | Unset):  Default: 1.
        page_size (int | Unset):  Default: 20.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | ListSandboxesResponse]
    """

    kwargs = _get_kwargs(
        state=state,
        metadata=metadata,
        page=page,
        page_size=page_size,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    *,
    client: AuthenticatedClient | Client,
    state: list[str] | Unset = UNSET,
    metadata: str | Unset = UNSET,
    page: int | Unset = 1,
    page_size: int | Unset = 20,
) -> ErrorResponse | ListSandboxesResponse | None:
    """List sandboxes

     List all sandboxes with optional filtering and pagination using query parameters.
    All filter conditions use AND logic. Multiple `state` parameters use OR logic within states.

    Args:
        state (list[str] | Unset):
        metadata (str | Unset):
        page (int | Unset):  Default: 1.
        page_size (int | Unset):  Default: 20.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | ListSandboxesResponse
    """

    return sync_detailed(
        client=client,
        state=state,
        metadata=metadata,
        page=page,
        page_size=page_size,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    state: list[str] | Unset = UNSET,
    metadata: str | Unset = UNSET,
    page: int | Unset = 1,
    page_size: int | Unset = 20,
) -> Response[ErrorResponse | ListSandboxesResponse]:
    """List sandboxes

     List all sandboxes with optional filtering and pagination using query parameters.
    All filter conditions use AND logic. Multiple `state` parameters use OR logic within states.

    Args:
        state (list[str] | Unset):
        metadata (str | Unset):
        page (int | Unset):  Default: 1.
        page_size (int | Unset):  Default: 20.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | ListSandboxesResponse]
    """

    kwargs = _get_kwargs(
        state=state,
        metadata=metadata,
        page=page,
        page_size=page_size,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    state: list[str] | Unset = UNSET,
    metadata: str | Unset = UNSET,
    page: int | Unset = 1,
    page_size: int | Unset = 20,
) -> ErrorResponse | ListSandboxesResponse | None:
    """List sandboxes

     List all sandboxes with optional filtering and pagination using query parameters.
    All filter conditions use AND logic. Multiple `state` parameters use OR logic within states.

    Args:
        state (list[str] | Unset):
        metadata (str | Unset):
        page (int | Unset):  Default: 1.
        page_size (int | Unset):  Default: 20.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | ListSandboxesResponse
    """

    return (
        await asyncio_detailed(
            client=client,
            state=state,
            metadata=metadata,
            page=page,
            page_size=page_size,
        )
    ).parsed
