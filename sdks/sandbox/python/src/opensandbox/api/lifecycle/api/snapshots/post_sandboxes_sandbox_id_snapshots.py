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
from ...models.create_snapshot_request import CreateSnapshotRequest
from ...models.error_response import ErrorResponse
from ...models.snapshot import Snapshot
from ...types import UNSET, Response, Unset


def _get_kwargs(
    sandbox_id: str,
    *,
    body: CreateSnapshotRequest | Unset = UNSET,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/sandboxes/{sandbox_id}/snapshots".format(
            sandbox_id=quote(str(sandbox_id), safe=""),
        ),
    }

    if not isinstance(body, Unset):
        _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> ErrorResponse | Snapshot | None:
    if response.status_code == 202:
        response_202 = Snapshot.from_dict(response.json())

        return response_202

    if response.status_code == 400:
        response_400 = ErrorResponse.from_dict(response.json())

        return response_400

    if response.status_code == 401:
        response_401 = ErrorResponse.from_dict(response.json())

        return response_401

    if response.status_code == 403:
        response_403 = ErrorResponse.from_dict(response.json())

        return response_403

    if response.status_code == 404:
        response_404 = ErrorResponse.from_dict(response.json())

        return response_404

    if response.status_code == 409:
        response_409 = ErrorResponse.from_dict(response.json())

        return response_409

    if response.status_code == 500:
        response_500 = ErrorResponse.from_dict(response.json())

        return response_500

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[ErrorResponse | Snapshot]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    sandbox_id: str,
    *,
    client: AuthenticatedClient | Client,
    body: CreateSnapshotRequest | Unset = UNSET,
) -> Response[ErrorResponse | Snapshot]:
    """Create a snapshot from a sandbox

     Create a persistent point-in-time snapshot from the sandbox's current state.
    The source sandbox must be `Running`. The returned snapshot id identifies
    the created artifact. Snapshot creation may temporarily pause the sandbox
    while the runtime captures provider-supported state, then the source
    sandbox continues running.

    Args:
        sandbox_id (str):
        body (CreateSnapshotRequest | Unset): Optional settings for creating a sandbox snapshot.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | Snapshot]
    """

    kwargs = _get_kwargs(
        sandbox_id=sandbox_id,
        body=body,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    sandbox_id: str,
    *,
    client: AuthenticatedClient | Client,
    body: CreateSnapshotRequest | Unset = UNSET,
) -> ErrorResponse | Snapshot | None:
    """Create a snapshot from a sandbox

     Create a persistent point-in-time snapshot from the sandbox's current state.
    The source sandbox must be `Running`. The returned snapshot id identifies
    the created artifact. Snapshot creation may temporarily pause the sandbox
    while the runtime captures provider-supported state, then the source
    sandbox continues running.

    Args:
        sandbox_id (str):
        body (CreateSnapshotRequest | Unset): Optional settings for creating a sandbox snapshot.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | Snapshot
    """

    return sync_detailed(
        sandbox_id=sandbox_id,
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    sandbox_id: str,
    *,
    client: AuthenticatedClient | Client,
    body: CreateSnapshotRequest | Unset = UNSET,
) -> Response[ErrorResponse | Snapshot]:
    """Create a snapshot from a sandbox

     Create a persistent point-in-time snapshot from the sandbox's current state.
    The source sandbox must be `Running`. The returned snapshot id identifies
    the created artifact. Snapshot creation may temporarily pause the sandbox
    while the runtime captures provider-supported state, then the source
    sandbox continues running.

    Args:
        sandbox_id (str):
        body (CreateSnapshotRequest | Unset): Optional settings for creating a sandbox snapshot.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | Snapshot]
    """

    kwargs = _get_kwargs(
        sandbox_id=sandbox_id,
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    sandbox_id: str,
    *,
    client: AuthenticatedClient | Client,
    body: CreateSnapshotRequest | Unset = UNSET,
) -> ErrorResponse | Snapshot | None:
    """Create a snapshot from a sandbox

     Create a persistent point-in-time snapshot from the sandbox's current state.
    The source sandbox must be `Running`. The returned snapshot id identifies
    the created artifact. Snapshot creation may temporarily pause the sandbox
    while the runtime captures provider-supported state, then the source
    sandbox continues running.

    Args:
        sandbox_id (str):
        body (CreateSnapshotRequest | Unset): Optional settings for creating a sandbox snapshot.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | Snapshot
    """

    return (
        await asyncio_detailed(
            sandbox_id=sandbox_id,
            client=client,
            body=body,
        )
    ).parsed
