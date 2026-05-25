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

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.create_sandbox_request import CreateSandboxRequest
from ...models.create_sandbox_response import CreateSandboxResponse
from ...models.error_response import ErrorResponse
from ...types import Response


def _get_kwargs(
    *,
    body: CreateSandboxRequest,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/sandboxes",
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> CreateSandboxResponse | ErrorResponse | None:
    if response.status_code == 202:
        response_202 = CreateSandboxResponse.from_dict(response.json())

        return response_202

    if response.status_code == 400:
        response_400 = ErrorResponse.from_dict(response.json())

        return response_400

    if response.status_code == 401:
        response_401 = ErrorResponse.from_dict(response.json())

        return response_401

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
) -> Response[CreateSandboxResponse | ErrorResponse]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: CreateSandboxRequest,
) -> Response[CreateSandboxResponse | ErrorResponse]:
    r"""Create a sandbox

     Creates a new sandbox from a container image or restores one from a
    persistent sandbox snapshot with optional resource limits, environment
    variables, and metadata.

    Exactly one startup source must be provided:
    - `image` to provision directly from a container image.
    - `snapshotId` to restore from a previously created snapshot.

    When `image` is provided, `entrypoint` is required. When `snapshotId` is
    provided, `entrypoint` is optional. If omitted, the server defaults the
    sandbox entrypoint to `[\"tail\", \"-f\", \"/dev/null\"]`.

    ## Authentication

    API Key authentication is required via:
    - `OPEN-SANDBOX-API-KEY: <api-key>` header

    Args:
        body (CreateSandboxRequest): Request to create a new sandbox from either a container image
            or a snapshot.
            Exactly one of `image` or `snapshotId` must be provided.

            When `image` is provided, `entrypoint` is required. When `snapshotId` is
            provided, `entrypoint` is optional. If omitted, the server defaults the
            sandbox entrypoint to `["tail", "-f", "/dev/null"]`.

            **Note**: API Key authentication is required via the `OPEN-SANDBOX-API-KEY` header.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[CreateSandboxResponse | ErrorResponse]
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
    body: CreateSandboxRequest,
) -> CreateSandboxResponse | ErrorResponse | None:
    r"""Create a sandbox

     Creates a new sandbox from a container image or restores one from a
    persistent sandbox snapshot with optional resource limits, environment
    variables, and metadata.

    Exactly one startup source must be provided:
    - `image` to provision directly from a container image.
    - `snapshotId` to restore from a previously created snapshot.

    When `image` is provided, `entrypoint` is required. When `snapshotId` is
    provided, `entrypoint` is optional. If omitted, the server defaults the
    sandbox entrypoint to `[\"tail\", \"-f\", \"/dev/null\"]`.

    ## Authentication

    API Key authentication is required via:
    - `OPEN-SANDBOX-API-KEY: <api-key>` header

    Args:
        body (CreateSandboxRequest): Request to create a new sandbox from either a container image
            or a snapshot.
            Exactly one of `image` or `snapshotId` must be provided.

            When `image` is provided, `entrypoint` is required. When `snapshotId` is
            provided, `entrypoint` is optional. If omitted, the server defaults the
            sandbox entrypoint to `["tail", "-f", "/dev/null"]`.

            **Note**: API Key authentication is required via the `OPEN-SANDBOX-API-KEY` header.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        CreateSandboxResponse | ErrorResponse
    """

    return sync_detailed(
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: CreateSandboxRequest,
) -> Response[CreateSandboxResponse | ErrorResponse]:
    r"""Create a sandbox

     Creates a new sandbox from a container image or restores one from a
    persistent sandbox snapshot with optional resource limits, environment
    variables, and metadata.

    Exactly one startup source must be provided:
    - `image` to provision directly from a container image.
    - `snapshotId` to restore from a previously created snapshot.

    When `image` is provided, `entrypoint` is required. When `snapshotId` is
    provided, `entrypoint` is optional. If omitted, the server defaults the
    sandbox entrypoint to `[\"tail\", \"-f\", \"/dev/null\"]`.

    ## Authentication

    API Key authentication is required via:
    - `OPEN-SANDBOX-API-KEY: <api-key>` header

    Args:
        body (CreateSandboxRequest): Request to create a new sandbox from either a container image
            or a snapshot.
            Exactly one of `image` or `snapshotId` must be provided.

            When `image` is provided, `entrypoint` is required. When `snapshotId` is
            provided, `entrypoint` is optional. If omitted, the server defaults the
            sandbox entrypoint to `["tail", "-f", "/dev/null"]`.

            **Note**: API Key authentication is required via the `OPEN-SANDBOX-API-KEY` header.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[CreateSandboxResponse | ErrorResponse]
    """

    kwargs = _get_kwargs(
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    body: CreateSandboxRequest,
) -> CreateSandboxResponse | ErrorResponse | None:
    r"""Create a sandbox

     Creates a new sandbox from a container image or restores one from a
    persistent sandbox snapshot with optional resource limits, environment
    variables, and metadata.

    Exactly one startup source must be provided:
    - `image` to provision directly from a container image.
    - `snapshotId` to restore from a previously created snapshot.

    When `image` is provided, `entrypoint` is required. When `snapshotId` is
    provided, `entrypoint` is optional. If omitted, the server defaults the
    sandbox entrypoint to `[\"tail\", \"-f\", \"/dev/null\"]`.

    ## Authentication

    API Key authentication is required via:
    - `OPEN-SANDBOX-API-KEY: <api-key>` header

    Args:
        body (CreateSandboxRequest): Request to create a new sandbox from either a container image
            or a snapshot.
            Exactly one of `image` or `snapshotId` must be provided.

            When `image` is provided, `entrypoint` is required. When `snapshotId` is
            provided, `entrypoint` is optional. If omitted, the server defaults the
            sandbox entrypoint to `["tail", "-f", "/dev/null"]`.

            **Note**: API Key authentication is required via the `OPEN-SANDBOX-API-KEY` header.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        CreateSandboxResponse | ErrorResponse
    """

    return (
        await asyncio_detailed(
            client=client,
            body=body,
        )
    ).parsed
