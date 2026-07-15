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
from ...models.credential_binding_metadata import CredentialBindingMetadata
from ...types import Response


def _get_kwargs(
    binding_name: str,
) -> dict[str, Any]:
    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/credential-vault/bindings/{binding_name}".format(
            binding_name=quote(str(binding_name), safe=""),
        ),
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> CredentialBindingMetadata | str | None:
    if response.status_code == 200:
        response_200 = CredentialBindingMetadata.from_dict(response.json())

        return response_200

    if response.status_code == 401:
        response_401 = response.text
        return response_401

    if response.status_code == 404:
        response_404 = response.text
        return response_404

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[CredentialBindingMetadata | str]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    binding_name: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[CredentialBindingMetadata | str]:
    """Get sanitized metadata for one binding

    Args:
        binding_name (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[CredentialBindingMetadata | str]
    """

    kwargs = _get_kwargs(
        binding_name=binding_name,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    binding_name: str,
    *,
    client: AuthenticatedClient | Client,
) -> CredentialBindingMetadata | str | None:
    """Get sanitized metadata for one binding

    Args:
        binding_name (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        CredentialBindingMetadata | str
    """

    return sync_detailed(
        binding_name=binding_name,
        client=client,
    ).parsed


async def asyncio_detailed(
    binding_name: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[CredentialBindingMetadata | str]:
    """Get sanitized metadata for one binding

    Args:
        binding_name (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[CredentialBindingMetadata | str]
    """

    kwargs = _get_kwargs(
        binding_name=binding_name,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    binding_name: str,
    *,
    client: AuthenticatedClient | Client,
) -> CredentialBindingMetadata | str | None:
    """Get sanitized metadata for one binding

    Args:
        binding_name (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        CredentialBindingMetadata | str
    """

    return (
        await asyncio_detailed(
            binding_name=binding_name,
            client=client,
        )
    ).parsed
