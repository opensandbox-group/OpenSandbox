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
from ...models.endpoint import Endpoint
from ...models.error_response import ErrorResponse
from ...types import UNSET, Response, Unset


def _get_kwargs(
    sandbox_id: str,
    port: int,
    *,
    use_server_proxy: bool | Unset = False,
    expires: str | Unset = UNSET,
) -> dict[str, Any]:
    params: dict[str, Any] = {}

    params["use_server_proxy"] = use_server_proxy

    params["expires"] = expires

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/sandboxes/{sandbox_id}/endpoints/{port}".format(
            sandbox_id=quote(str(sandbox_id), safe=""),
            port=quote(str(port), safe=""),
        ),
        "params": params,
    }

    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Endpoint | ErrorResponse | None:
    if response.status_code == 200:
        response_200 = Endpoint.from_dict(response.json())

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

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[Endpoint | ErrorResponse]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    sandbox_id: str,
    port: int,
    *,
    client: AuthenticatedClient | Client,
    use_server_proxy: bool | Unset = False,
    expires: str | Unset = UNSET,
) -> Response[Endpoint | ErrorResponse]:
    """Get sandbox access endpoint

     Get the public access endpoint URL for accessing a service running on a specific port
    within the sandbox. The service must be listening on the specified port inside
    the sandbox for the endpoint to be available.

    Args:
        sandbox_id (str):
        port (int):
        use_server_proxy (bool | Unset):  Default: False.
        expires (str | Unset):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Endpoint | ErrorResponse]
    """

    kwargs = _get_kwargs(
        sandbox_id=sandbox_id,
        port=port,
        use_server_proxy=use_server_proxy,
        expires=expires,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    sandbox_id: str,
    port: int,
    *,
    client: AuthenticatedClient | Client,
    use_server_proxy: bool | Unset = False,
    expires: str | Unset = UNSET,
) -> Endpoint | ErrorResponse | None:
    """Get sandbox access endpoint

     Get the public access endpoint URL for accessing a service running on a specific port
    within the sandbox. The service must be listening on the specified port inside
    the sandbox for the endpoint to be available.

    Args:
        sandbox_id (str):
        port (int):
        use_server_proxy (bool | Unset):  Default: False.
        expires (str | Unset):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Endpoint | ErrorResponse
    """

    return sync_detailed(
        sandbox_id=sandbox_id,
        port=port,
        client=client,
        use_server_proxy=use_server_proxy,
        expires=expires,
    ).parsed


async def asyncio_detailed(
    sandbox_id: str,
    port: int,
    *,
    client: AuthenticatedClient | Client,
    use_server_proxy: bool | Unset = False,
    expires: str | Unset = UNSET,
) -> Response[Endpoint | ErrorResponse]:
    """Get sandbox access endpoint

     Get the public access endpoint URL for accessing a service running on a specific port
    within the sandbox. The service must be listening on the specified port inside
    the sandbox for the endpoint to be available.

    Args:
        sandbox_id (str):
        port (int):
        use_server_proxy (bool | Unset):  Default: False.
        expires (str | Unset):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Endpoint | ErrorResponse]
    """

    kwargs = _get_kwargs(
        sandbox_id=sandbox_id,
        port=port,
        use_server_proxy=use_server_proxy,
        expires=expires,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    sandbox_id: str,
    port: int,
    *,
    client: AuthenticatedClient | Client,
    use_server_proxy: bool | Unset = False,
    expires: str | Unset = UNSET,
) -> Endpoint | ErrorResponse | None:
    """Get sandbox access endpoint

     Get the public access endpoint URL for accessing a service running on a specific port
    within the sandbox. The service must be listening on the specified port inside
    the sandbox for the endpoint to be available.

    Args:
        sandbox_id (str):
        port (int):
        use_server_proxy (bool | Unset):  Default: False.
        expires (str | Unset):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Endpoint | ErrorResponse
    """

    return (
        await asyncio_detailed(
            sandbox_id=sandbox_id,
            port=port,
            client=client,
            use_server_proxy=use_server_proxy,
            expires=expires,
        )
    ).parsed
