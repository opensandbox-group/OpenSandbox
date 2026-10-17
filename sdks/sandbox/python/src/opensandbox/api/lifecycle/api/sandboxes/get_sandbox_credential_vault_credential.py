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
from ...models.credential_metadata import CredentialMetadata
from ...models.error_response import ErrorResponse
from ...types import Response


def _get_kwargs(
    sandbox_id: str,
    credential_name: str,
) -> dict[str, Any]:
    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/sandboxes/{sandbox_id}/credential-vault/credentials/{credential_name}".format(
            sandbox_id=quote(str(sandbox_id), safe=""),
            credential_name=quote(str(credential_name), safe=""),
        ),
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> CredentialMetadata | ErrorResponse | None:
    if response.status_code == 200:
        response_200 = CredentialMetadata.from_dict(response.json())

        return response_200

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

    if response.status_code == 503:
        response_503 = ErrorResponse.from_dict(response.json())

        return response_503

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[CredentialMetadata | ErrorResponse]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    sandbox_id: str,
    credential_name: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[CredentialMetadata | ErrorResponse]:
    """Get sanitized metadata for one credential

     Returns sanitized metadata for a single credential; values are never
    returned. Other backends proxy the sandbox-side egress service.

    Args:
        sandbox_id (str):
        credential_name (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[CredentialMetadata | ErrorResponse]
    """

    kwargs = _get_kwargs(
        sandbox_id=sandbox_id,
        credential_name=credential_name,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    sandbox_id: str,
    credential_name: str,
    *,
    client: AuthenticatedClient | Client,
) -> CredentialMetadata | ErrorResponse | None:
    """Get sanitized metadata for one credential

     Returns sanitized metadata for a single credential; values are never
    returned. Other backends proxy the sandbox-side egress service.

    Args:
        sandbox_id (str):
        credential_name (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        CredentialMetadata | ErrorResponse
    """

    return sync_detailed(
        sandbox_id=sandbox_id,
        credential_name=credential_name,
        client=client,
    ).parsed


async def asyncio_detailed(
    sandbox_id: str,
    credential_name: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[CredentialMetadata | ErrorResponse]:
    """Get sanitized metadata for one credential

     Returns sanitized metadata for a single credential; values are never
    returned. Other backends proxy the sandbox-side egress service.

    Args:
        sandbox_id (str):
        credential_name (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[CredentialMetadata | ErrorResponse]
    """

    kwargs = _get_kwargs(
        sandbox_id=sandbox_id,
        credential_name=credential_name,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    sandbox_id: str,
    credential_name: str,
    *,
    client: AuthenticatedClient | Client,
) -> CredentialMetadata | ErrorResponse | None:
    """Get sanitized metadata for one credential

     Returns sanitized metadata for a single credential; values are never
    returned. Other backends proxy the sandbox-side egress service.

    Args:
        sandbox_id (str):
        credential_name (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        CredentialMetadata | ErrorResponse
    """

    return (
        await asyncio_detailed(
            sandbox_id=sandbox_id,
            credential_name=credential_name,
            client=client,
        )
    ).parsed
