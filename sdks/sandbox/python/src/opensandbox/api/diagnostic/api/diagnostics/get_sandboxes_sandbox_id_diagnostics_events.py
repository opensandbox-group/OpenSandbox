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
from ...models.diagnostic_content_response import DiagnosticContentResponse
from ...models.error_response import ErrorResponse
from ...types import UNSET, Response


def _get_kwargs(
    sandbox_id: str,
    *,
    scope: str,
) -> dict[str, Any]:
    params: dict[str, Any] = {}

    params["scope"] = scope

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/sandboxes/{sandbox_id}/diagnostics/events".format(
            sandbox_id=quote(str(sandbox_id), safe=""),
        ),
        "params": params,
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> DiagnosticContentResponse | ErrorResponse | None:
    if response.status_code == 200:
        response_200 = DiagnosticContentResponse.from_dict(response.json())

        return response_200

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

    if response.status_code == 500:
        response_500 = ErrorResponse.from_dict(response.json())

        return response_500

    if response.status_code == 501:
        response_501 = ErrorResponse.from_dict(response.json())

        return response_501

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[DiagnosticContentResponse | ErrorResponse]:
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
    scope: str,
) -> Response[DiagnosticContentResponse | ErrorResponse]:
    """Get diagnostic events

     Retrieve a best-effort descriptor for sandbox diagnostic event text.

    Events are rendered as diagnostic text rather than exposed as a stable
    structured event model. Depending on the selected `scope` and runtime, event
    text may include lifecycle transitions, runtime or platform events such as
    Kubernetes Events, network diagnostic events, process activity events, or
    other implementation-defined event material.

    This endpoint does not provide streaming, pagination, or a stable line-level
    schema in this version. The server returns a JSON descriptor for currently
    available diagnostic event text for the requested scope, subject to
    implementation-defined retention and response size limits. The descriptor
    either embeds the text as `content` or returns a `contentUrl` where the
    text can be downloaded.

    Args:
        sandbox_id (str):
        scope (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[DiagnosticContentResponse | ErrorResponse]
    """

    kwargs = _get_kwargs(
        sandbox_id=sandbox_id,
        scope=scope,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    sandbox_id: str,
    *,
    client: AuthenticatedClient | Client,
    scope: str,
) -> DiagnosticContentResponse | ErrorResponse | None:
    """Get diagnostic events

     Retrieve a best-effort descriptor for sandbox diagnostic event text.

    Events are rendered as diagnostic text rather than exposed as a stable
    structured event model. Depending on the selected `scope` and runtime, event
    text may include lifecycle transitions, runtime or platform events such as
    Kubernetes Events, network diagnostic events, process activity events, or
    other implementation-defined event material.

    This endpoint does not provide streaming, pagination, or a stable line-level
    schema in this version. The server returns a JSON descriptor for currently
    available diagnostic event text for the requested scope, subject to
    implementation-defined retention and response size limits. The descriptor
    either embeds the text as `content` or returns a `contentUrl` where the
    text can be downloaded.

    Args:
        sandbox_id (str):
        scope (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        DiagnosticContentResponse | ErrorResponse
    """

    return sync_detailed(
        sandbox_id=sandbox_id,
        client=client,
        scope=scope,
    ).parsed


async def asyncio_detailed(
    sandbox_id: str,
    *,
    client: AuthenticatedClient | Client,
    scope: str,
) -> Response[DiagnosticContentResponse | ErrorResponse]:
    """Get diagnostic events

     Retrieve a best-effort descriptor for sandbox diagnostic event text.

    Events are rendered as diagnostic text rather than exposed as a stable
    structured event model. Depending on the selected `scope` and runtime, event
    text may include lifecycle transitions, runtime or platform events such as
    Kubernetes Events, network diagnostic events, process activity events, or
    other implementation-defined event material.

    This endpoint does not provide streaming, pagination, or a stable line-level
    schema in this version. The server returns a JSON descriptor for currently
    available diagnostic event text for the requested scope, subject to
    implementation-defined retention and response size limits. The descriptor
    either embeds the text as `content` or returns a `contentUrl` where the
    text can be downloaded.

    Args:
        sandbox_id (str):
        scope (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[DiagnosticContentResponse | ErrorResponse]
    """

    kwargs = _get_kwargs(
        sandbox_id=sandbox_id,
        scope=scope,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    sandbox_id: str,
    *,
    client: AuthenticatedClient | Client,
    scope: str,
) -> DiagnosticContentResponse | ErrorResponse | None:
    """Get diagnostic events

     Retrieve a best-effort descriptor for sandbox diagnostic event text.

    Events are rendered as diagnostic text rather than exposed as a stable
    structured event model. Depending on the selected `scope` and runtime, event
    text may include lifecycle transitions, runtime or platform events such as
    Kubernetes Events, network diagnostic events, process activity events, or
    other implementation-defined event material.

    This endpoint does not provide streaming, pagination, or a stable line-level
    schema in this version. The server returns a JSON descriptor for currently
    available diagnostic event text for the requested scope, subject to
    implementation-defined retention and response size limits. The descriptor
    either embeds the text as `content` or returns a `contentUrl` where the
    text can be downloaded.

    Args:
        sandbox_id (str):
        scope (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        DiagnosticContentResponse | ErrorResponse
    """

    return (
        await asyncio_detailed(
            sandbox_id=sandbox_id,
            client=client,
            scope=scope,
        )
    ).parsed
