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
from ...models.credential_vault_mutation_request import CredentialVaultMutationRequest
from ...models.credential_vault_state import CredentialVaultState
from ...models.error_response import ErrorResponse
from ...types import Response


def _get_kwargs(
    sandbox_id: str,
    *,
    body: CredentialVaultMutationRequest,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "patch",
        "url": "/sandboxes/{sandbox_id}/credential-vault".format(
            sandbox_id=quote(str(sandbox_id), safe=""),
        ),
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> CredentialVaultState | ErrorResponse | None:
    if response.status_code == 200:
        response_200 = CredentialVaultState.from_dict(response.json())

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

    if response.status_code == 409:
        response_409 = ErrorResponse.from_dict(response.json())

        return response_409

    if response.status_code == 412:
        response_412 = ErrorResponse.from_dict(response.json())

        return response_412

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
) -> Response[CredentialVaultState | ErrorResponse]:
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
    body: CredentialVaultMutationRequest,
) -> Response[CredentialVaultState | ErrorResponse]:
    """Atomically mutate sandbox credentials and bindings

     Applies one atomic mutation set: `delete` first, then `replace`, then
    `add` (per set). Every mentioned name must resolve exactly once —
    `add` requires the name to be absent; `replace` and `delete` require
    it to exist. `expectedRevision`, when provided, must match the
    current revision (412 otherwise).

    For Fsb the merge is composed server-side (relay read → in-memory
    merge → relay replace) and validated by the egress vault; a rejected
    mutation never leaves a partially applied revision (last-good kept).
    Other backends proxy the sandbox-side egress service. On 412, re-read
    the state and retry with the fresh revision.

    Args:
        sandbox_id (str):
        body (CredentialVaultMutationRequest): One atomic mutation set. Applied per set as delete
            → replace → add;
            every mentioned name must resolve exactly once (add requires the
            name to be absent, replace and delete require it to exist).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[CredentialVaultState | ErrorResponse]
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
    body: CredentialVaultMutationRequest,
) -> CredentialVaultState | ErrorResponse | None:
    """Atomically mutate sandbox credentials and bindings

     Applies one atomic mutation set: `delete` first, then `replace`, then
    `add` (per set). Every mentioned name must resolve exactly once —
    `add` requires the name to be absent; `replace` and `delete` require
    it to exist. `expectedRevision`, when provided, must match the
    current revision (412 otherwise).

    For Fsb the merge is composed server-side (relay read → in-memory
    merge → relay replace) and validated by the egress vault; a rejected
    mutation never leaves a partially applied revision (last-good kept).
    Other backends proxy the sandbox-side egress service. On 412, re-read
    the state and retry with the fresh revision.

    Args:
        sandbox_id (str):
        body (CredentialVaultMutationRequest): One atomic mutation set. Applied per set as delete
            → replace → add;
            every mentioned name must resolve exactly once (add requires the
            name to be absent, replace and delete require it to exist).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        CredentialVaultState | ErrorResponse
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
    body: CredentialVaultMutationRequest,
) -> Response[CredentialVaultState | ErrorResponse]:
    """Atomically mutate sandbox credentials and bindings

     Applies one atomic mutation set: `delete` first, then `replace`, then
    `add` (per set). Every mentioned name must resolve exactly once —
    `add` requires the name to be absent; `replace` and `delete` require
    it to exist. `expectedRevision`, when provided, must match the
    current revision (412 otherwise).

    For Fsb the merge is composed server-side (relay read → in-memory
    merge → relay replace) and validated by the egress vault; a rejected
    mutation never leaves a partially applied revision (last-good kept).
    Other backends proxy the sandbox-side egress service. On 412, re-read
    the state and retry with the fresh revision.

    Args:
        sandbox_id (str):
        body (CredentialVaultMutationRequest): One atomic mutation set. Applied per set as delete
            → replace → add;
            every mentioned name must resolve exactly once (add requires the
            name to be absent, replace and delete require it to exist).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[CredentialVaultState | ErrorResponse]
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
    body: CredentialVaultMutationRequest,
) -> CredentialVaultState | ErrorResponse | None:
    """Atomically mutate sandbox credentials and bindings

     Applies one atomic mutation set: `delete` first, then `replace`, then
    `add` (per set). Every mentioned name must resolve exactly once —
    `add` requires the name to be absent; `replace` and `delete` require
    it to exist. `expectedRevision`, when provided, must match the
    current revision (412 otherwise).

    For Fsb the merge is composed server-side (relay read → in-memory
    merge → relay replace) and validated by the egress vault; a rejected
    mutation never leaves a partially applied revision (last-good kept).
    Other backends proxy the sandbox-side egress service. On 412, re-read
    the state and retry with the fresh revision.

    Args:
        sandbox_id (str):
        body (CredentialVaultMutationRequest): One atomic mutation set. Applied per set as delete
            → replace → add;
            every mentioned name must resolve exactly once (add requires the
            name to be absent, replace and delete require it to exist).

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        CredentialVaultState | ErrorResponse
    """

    return (
        await asyncio_detailed(
            sandbox_id=sandbox_id,
            client=client,
            body=body,
        )
    ).parsed
