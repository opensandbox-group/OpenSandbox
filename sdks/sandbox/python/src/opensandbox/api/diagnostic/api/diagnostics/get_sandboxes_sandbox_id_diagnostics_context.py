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
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.diagnostic_context_response import DiagnosticContextResponse
from ...models.error_response import ErrorResponse
from ...types import Response


def _get_kwargs(
    sandbox_id: str,
) -> dict[str, Any]:
    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/sandboxes/{sandbox_id}/diagnostics/context".format(
            sandbox_id=quote(str(sandbox_id), safe=""),
        ),
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> DiagnosticContextResponse | ErrorResponse | None:
    if response.status_code == 200:
        response_200 = DiagnosticContextResponse.from_dict(response.json())

        return response_200

    if response.status_code == 401:
        try:
            response_401 = ErrorResponse.from_dict(response.json())
        except (ValueError, TypeError, KeyError):
            return None

        return response_401

    if response.status_code == 403:
        try:
            response_403 = ErrorResponse.from_dict(response.json())
        except (ValueError, TypeError, KeyError):
            return None

        return response_403

    if response.status_code == 404:
        try:
            response_404 = ErrorResponse.from_dict(response.json())
        except (ValueError, TypeError, KeyError):
            return None

        return response_404

    if response.status_code == 500:
        try:
            response_500 = ErrorResponse.from_dict(response.json())
        except (ValueError, TypeError, KeyError):
            return None

        return response_500

    if response.status_code == 501:
        try:
            response_501 = ErrorResponse.from_dict(response.json())
        except (ValueError, TypeError, KeyError):
            return None

        return response_501

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[DiagnosticContextResponse | ErrorResponse]:
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
) -> Response[DiagnosticContextResponse | ErrorResponse]:
    """Get diagnostic context

     Retrieve structured context for routing further diagnostics of a sandbox.

    `backendType` identifies the implementation that currently owns the
    sandbox. `backendIdentifiers` contains implementation-defined identifiers
    that may be used to correlate the sandbox with resources in that backend.
    Identifier keys are scoped to the backend type and are not defined by this
    API. The map may be empty when the backend does not expose additional
    identifiers.

    This endpoint does not return diagnostic logs or events. Use the dedicated
    `logs` and `events` endpoints after resolving the diagnostic context.

    Args:
        sandbox_id (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[DiagnosticContextResponse | ErrorResponse]
    """

    kwargs = _get_kwargs(
        sandbox_id=sandbox_id,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    sandbox_id: str,
    *,
    client: AuthenticatedClient | Client,
) -> DiagnosticContextResponse | ErrorResponse | None:
    """Get diagnostic context

     Retrieve structured context for routing further diagnostics of a sandbox.

    `backendType` identifies the implementation that currently owns the
    sandbox. `backendIdentifiers` contains implementation-defined identifiers
    that may be used to correlate the sandbox with resources in that backend.
    Identifier keys are scoped to the backend type and are not defined by this
    API. The map may be empty when the backend does not expose additional
    identifiers.

    This endpoint does not return diagnostic logs or events. Use the dedicated
    `logs` and `events` endpoints after resolving the diagnostic context.

    Args:
        sandbox_id (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        DiagnosticContextResponse | ErrorResponse
    """

    return sync_detailed(
        sandbox_id=sandbox_id,
        client=client,
    ).parsed


async def asyncio_detailed(
    sandbox_id: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[DiagnosticContextResponse | ErrorResponse]:
    """Get diagnostic context

     Retrieve structured context for routing further diagnostics of a sandbox.

    `backendType` identifies the implementation that currently owns the
    sandbox. `backendIdentifiers` contains implementation-defined identifiers
    that may be used to correlate the sandbox with resources in that backend.
    Identifier keys are scoped to the backend type and are not defined by this
    API. The map may be empty when the backend does not expose additional
    identifiers.

    This endpoint does not return diagnostic logs or events. Use the dedicated
    `logs` and `events` endpoints after resolving the diagnostic context.

    Args:
        sandbox_id (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[DiagnosticContextResponse | ErrorResponse]
    """

    kwargs = _get_kwargs(
        sandbox_id=sandbox_id,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    sandbox_id: str,
    *,
    client: AuthenticatedClient | Client,
) -> DiagnosticContextResponse | ErrorResponse | None:
    """Get diagnostic context

     Retrieve structured context for routing further diagnostics of a sandbox.

    `backendType` identifies the implementation that currently owns the
    sandbox. `backendIdentifiers` contains implementation-defined identifiers
    that may be used to correlate the sandbox with resources in that backend.
    Identifier keys are scoped to the backend type and are not defined by this
    API. The map may be empty when the backend does not expose additional
    identifiers.

    This endpoint does not return diagnostic logs or events. Use the dedicated
    `logs` and `events` endpoints after resolving the diagnostic context.

    Args:
        sandbox_id (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        DiagnosticContextResponse | ErrorResponse
    """

    return (
        await asyncio_detailed(
            sandbox_id=sandbox_id,
            client=client,
        )
    ).parsed
