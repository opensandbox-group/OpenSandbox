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
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error_response import ErrorResponse
from ...types import Response


def _get_kwargs(
    snapshot_id: str,
) -> dict[str, Any]:
    _kwargs: dict[str, Any] = {
        "method": "delete",
        "url": "/snapshots/{snapshot_id}".format(
            snapshot_id=quote(str(snapshot_id), safe=""),
        ),
    }

    return _kwargs


def _parse_response(*, client: AuthenticatedClient | Client, response: httpx.Response) -> Any | ErrorResponse | None:
    if response.status_code == 204:
        response_204 = cast(Any, None)
        return response_204

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


def _build_response(*, client: AuthenticatedClient | Client, response: httpx.Response) -> Response[Any | ErrorResponse]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    snapshot_id: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[Any | ErrorResponse]:
    """Delete a snapshot

     Delete a persistent sandbox snapshot by id. Snapshots that are still
    being created cannot be deleted.

    For Kubernetes-backed snapshots, deletion removes OpenSandbox metadata
    and Kubernetes coordination resources, but does not guarantee removal
    of pushed OCI images from the configured registry. Use registry
    retention or garbage collection policies for image lifecycle cleanup.

    Args:
        snapshot_id (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | ErrorResponse]
    """

    kwargs = _get_kwargs(
        snapshot_id=snapshot_id,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    snapshot_id: str,
    *,
    client: AuthenticatedClient | Client,
) -> Any | ErrorResponse | None:
    """Delete a snapshot

     Delete a persistent sandbox snapshot by id. Snapshots that are still
    being created cannot be deleted.

    For Kubernetes-backed snapshots, deletion removes OpenSandbox metadata
    and Kubernetes coordination resources, but does not guarantee removal
    of pushed OCI images from the configured registry. Use registry
    retention or garbage collection policies for image lifecycle cleanup.

    Args:
        snapshot_id (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | ErrorResponse
    """

    return sync_detailed(
        snapshot_id=snapshot_id,
        client=client,
    ).parsed


async def asyncio_detailed(
    snapshot_id: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[Any | ErrorResponse]:
    """Delete a snapshot

     Delete a persistent sandbox snapshot by id. Snapshots that are still
    being created cannot be deleted.

    For Kubernetes-backed snapshots, deletion removes OpenSandbox metadata
    and Kubernetes coordination resources, but does not guarantee removal
    of pushed OCI images from the configured registry. Use registry
    retention or garbage collection policies for image lifecycle cleanup.

    Args:
        snapshot_id (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | ErrorResponse]
    """

    kwargs = _get_kwargs(
        snapshot_id=snapshot_id,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    snapshot_id: str,
    *,
    client: AuthenticatedClient | Client,
) -> Any | ErrorResponse | None:
    """Delete a snapshot

     Delete a persistent sandbox snapshot by id. Snapshots that are still
    being created cannot be deleted.

    For Kubernetes-backed snapshots, deletion removes OpenSandbox metadata
    and Kubernetes coordination resources, but does not guarantee removal
    of pushed OCI images from the configured registry. Use registry
    retention or garbage collection policies for image lifecycle cleanup.

    Args:
        snapshot_id (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | ErrorResponse
    """

    return (
        await asyncio_detailed(
            snapshot_id=snapshot_id,
            client=client,
        )
    ).parsed
