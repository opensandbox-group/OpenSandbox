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
from ...models.create_fsb_template_request import CreateFsbTemplateRequest
from ...models.error_response import ErrorResponse
from ...models.fsb_template import FsbTemplate
from ...types import Response


def _get_kwargs(
    *,
    body: CreateFsbTemplateRequest,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/templates",
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> ErrorResponse | FsbTemplate | None:
    if response.status_code == 201:
        response_201 = FsbTemplate.from_dict(response.json())

        return response_201

    if response.status_code == 400:
        response_400 = ErrorResponse.from_dict(response.json())

        return response_400

    if response.status_code == 401:
        response_401 = ErrorResponse.from_dict(response.json())

        return response_401

    if response.status_code == 409:
        response_409 = ErrorResponse.from_dict(response.json())

        return response_409

    if response.status_code == 501:
        response_501 = ErrorResponse.from_dict(response.json())

        return response_501

    if response.status_code == 503:
        response_503 = ErrorResponse.from_dict(response.json())

        return response_503

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[ErrorResponse | FsbTemplate]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: CreateFsbTemplateRequest,
) -> Response[ErrorResponse | FsbTemplate]:
    """Create a fsb template

     Declares a golden-image build executed by fast-sandbox.
    Available on Kubernetes-backed runtimes (`kubernetes` and `fsb`);
    501 otherwise (e.g. the Docker runtime).

    The build is asynchronous: the response carries `status.phase: Pending`;
    poll `GET /templates/{templateId}` until `Succeeded` (or `Failed` with
    `status.message`). Only `Succeeded` templates can back template-based
    sandbox creation. Kernel, execd and guest init are server-side build
    inputs supplied from the `[fsb]` configuration, not client fields.

    Templates are tenant-private: every template is scoped to the
    requester's fast-sandbox namespace.

    Args:
        body (CreateFsbTemplateRequest): Request to create a Fast Sandbox template: a fast-sandbox
            golden-image build
            The server persists the build intent, projects it onto a
            SandboxTemplate CRD, and reports the asynchronous build through the
            template status. Kernel, execd and guest init are server-side build
            inputs supplied from the `[fsb]` configuration, not client fields.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | FsbTemplate]
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
    body: CreateFsbTemplateRequest,
) -> ErrorResponse | FsbTemplate | None:
    """Create a fsb template

     Declares a golden-image build executed by fast-sandbox.
    Available on Kubernetes-backed runtimes (`kubernetes` and `fsb`);
    501 otherwise (e.g. the Docker runtime).

    The build is asynchronous: the response carries `status.phase: Pending`;
    poll `GET /templates/{templateId}` until `Succeeded` (or `Failed` with
    `status.message`). Only `Succeeded` templates can back template-based
    sandbox creation. Kernel, execd and guest init are server-side build
    inputs supplied from the `[fsb]` configuration, not client fields.

    Templates are tenant-private: every template is scoped to the
    requester's fast-sandbox namespace.

    Args:
        body (CreateFsbTemplateRequest): Request to create a Fast Sandbox template: a fast-sandbox
            golden-image build
            The server persists the build intent, projects it onto a
            SandboxTemplate CRD, and reports the asynchronous build through the
            template status. Kernel, execd and guest init are server-side build
            inputs supplied from the `[fsb]` configuration, not client fields.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | FsbTemplate
    """

    return sync_detailed(
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: CreateFsbTemplateRequest,
) -> Response[ErrorResponse | FsbTemplate]:
    """Create a fsb template

     Declares a golden-image build executed by fast-sandbox.
    Available on Kubernetes-backed runtimes (`kubernetes` and `fsb`);
    501 otherwise (e.g. the Docker runtime).

    The build is asynchronous: the response carries `status.phase: Pending`;
    poll `GET /templates/{templateId}` until `Succeeded` (or `Failed` with
    `status.message`). Only `Succeeded` templates can back template-based
    sandbox creation. Kernel, execd and guest init are server-side build
    inputs supplied from the `[fsb]` configuration, not client fields.

    Templates are tenant-private: every template is scoped to the
    requester's fast-sandbox namespace.

    Args:
        body (CreateFsbTemplateRequest): Request to create a Fast Sandbox template: a fast-sandbox
            golden-image build
            The server persists the build intent, projects it onto a
            SandboxTemplate CRD, and reports the asynchronous build through the
            template status. Kernel, execd and guest init are server-side build
            inputs supplied from the `[fsb]` configuration, not client fields.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | FsbTemplate]
    """

    kwargs = _get_kwargs(
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    body: CreateFsbTemplateRequest,
) -> ErrorResponse | FsbTemplate | None:
    """Create a fsb template

     Declares a golden-image build executed by fast-sandbox.
    Available on Kubernetes-backed runtimes (`kubernetes` and `fsb`);
    501 otherwise (e.g. the Docker runtime).

    The build is asynchronous: the response carries `status.phase: Pending`;
    poll `GET /templates/{templateId}` until `Succeeded` (or `Failed` with
    `status.message`). Only `Succeeded` templates can back template-based
    sandbox creation. Kernel, execd and guest init are server-side build
    inputs supplied from the `[fsb]` configuration, not client fields.

    Templates are tenant-private: every template is scoped to the
    requester's fast-sandbox namespace.

    Args:
        body (CreateFsbTemplateRequest): Request to create a Fast Sandbox template: a fast-sandbox
            golden-image build
            The server persists the build intent, projects it onto a
            SandboxTemplate CRD, and reports the asynchronous build through the
            template status. Kernel, execd and guest init are server-side build
            inputs supplied from the `[fsb]` configuration, not client fields.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | FsbTemplate
    """

    return (
        await asyncio_detailed(
            client=client,
            body=body,
        )
    ).parsed
