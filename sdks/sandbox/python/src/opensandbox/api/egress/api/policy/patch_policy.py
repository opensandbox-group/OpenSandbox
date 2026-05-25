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
from ...models.network_rule import NetworkRule
from ...models.policy_status_response import PolicyStatusResponse
from ...types import Response


def _get_kwargs(
    *,
    body: list[NetworkRule],
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "patch",
        "url": "/policy",
    }

    _kwargs["json"] = []
    for body_item_data in body:
        body_item = body_item_data.to_dict()
        _kwargs["json"].append(body_item)

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> PolicyStatusResponse | str | None:
    if response.status_code == 200:
        response_200 = PolicyStatusResponse.from_dict(response.json())

        return response_200

    if response.status_code == 400:
        response_400 = response.text
        return response_400

    if response.status_code == 401:
        response_401 = response.text
        return response_401

    if response.status_code == 500:
        response_500 = response.text
        return response_500

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[PolicyStatusResponse | str]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: list[NetworkRule],
) -> Response[PolicyStatusResponse | str]:
    """Patch egress rules

     Merge incoming egress rules with the currently enforced policy.

    This endpoint uses merge semantics:
    - Existing rules remain unless overridden by incoming rules.
    - Incoming rules are applied with higher priority than existing rules.
    - If multiple incoming rules refer to the same `target`, the first one wins.

    Args:
        body (list[NetworkRule]):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[PolicyStatusResponse | str]
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
    body: list[NetworkRule],
) -> PolicyStatusResponse | str | None:
    """Patch egress rules

     Merge incoming egress rules with the currently enforced policy.

    This endpoint uses merge semantics:
    - Existing rules remain unless overridden by incoming rules.
    - Incoming rules are applied with higher priority than existing rules.
    - If multiple incoming rules refer to the same `target`, the first one wins.

    Args:
        body (list[NetworkRule]):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        PolicyStatusResponse | str
    """

    return sync_detailed(
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: list[NetworkRule],
) -> Response[PolicyStatusResponse | str]:
    """Patch egress rules

     Merge incoming egress rules with the currently enforced policy.

    This endpoint uses merge semantics:
    - Existing rules remain unless overridden by incoming rules.
    - Incoming rules are applied with higher priority than existing rules.
    - If multiple incoming rules refer to the same `target`, the first one wins.

    Args:
        body (list[NetworkRule]):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[PolicyStatusResponse | str]
    """

    kwargs = _get_kwargs(
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    body: list[NetworkRule],
) -> PolicyStatusResponse | str | None:
    """Patch egress rules

     Merge incoming egress rules with the currently enforced policy.

    This endpoint uses merge semantics:
    - Existing rules remain unless overridden by incoming rules.
    - Incoming rules are applied with higher priority than existing rules.
    - If multiple incoming rules refer to the same `target`, the first one wins.

    Args:
        body (list[NetworkRule]):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        PolicyStatusResponse | str
    """

    return (
        await asyncio_detailed(
            client=client,
            body=body,
        )
    ).parsed
