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
from typing import Any, cast
from urllib.parse import quote

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.egress_signing_jwk import EgressSigningJwk
from ...types import Response


def _get_kwargs(
    kid: str,
) -> dict[str, Any]:
    _kwargs: dict[str, Any] = {
        "method": "get",
        "url": "/credential-vault/jwks/{kid}".format(
            kid=quote(str(kid), safe=""),
        ),
    }

    return _kwargs


def _parse_response(*, client: AuthenticatedClient | Client, response: httpx.Response) -> Any | EgressSigningJwk | None:
    if response.status_code == 200:
        response_200 = EgressSigningJwk.from_dict(response.json())

        return response_200

    if response.status_code == 404:
        response_404 = cast(Any, None)
        return response_404

    if response.status_code == 501:
        response_501 = cast(Any, None)
        return response_501

    if response.status_code == 503:
        response_503 = cast(Any, None)
        return response_503

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[Any | EgressSigningJwk]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    kid: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[Any | EgressSigningJwk]:
    """Publish one credential-vault resolver caller-authentication public key

     Credential-vault resolver caller authentication: credential
    resolvers verify the `OpenSandbox-Egress-Credential` JWT offline
    against the Ed25519 public key identified by `kid`.

    The endpoint is public by design — it serves public keys only (the
    OIDC `/.well-known/jwks.json` posture) and requires no API key; abuse
    is bounded by local `kid` grammar validation and a per-window lookup
    budget. `kid` is a platform identifier: a fast-sandbox pool kid
    (`<pool>:v<version>`) or a sidecar sandbox's sandboxId.
    Off-grammar and unknown kids answer 404 without a backend read.

    Args:
        kid (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | EgressSigningJwk]
    """

    kwargs = _get_kwargs(
        kid=kid,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    kid: str,
    *,
    client: AuthenticatedClient | Client,
) -> Any | EgressSigningJwk | None:
    """Publish one credential-vault resolver caller-authentication public key

     Credential-vault resolver caller authentication: credential
    resolvers verify the `OpenSandbox-Egress-Credential` JWT offline
    against the Ed25519 public key identified by `kid`.

    The endpoint is public by design — it serves public keys only (the
    OIDC `/.well-known/jwks.json` posture) and requires no API key; abuse
    is bounded by local `kid` grammar validation and a per-window lookup
    budget. `kid` is a platform identifier: a fast-sandbox pool kid
    (`<pool>:v<version>`) or a sidecar sandbox's sandboxId.
    Off-grammar and unknown kids answer 404 without a backend read.

    Args:
        kid (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | EgressSigningJwk
    """

    return sync_detailed(
        kid=kid,
        client=client,
    ).parsed


async def asyncio_detailed(
    kid: str,
    *,
    client: AuthenticatedClient | Client,
) -> Response[Any | EgressSigningJwk]:
    """Publish one credential-vault resolver caller-authentication public key

     Credential-vault resolver caller authentication: credential
    resolvers verify the `OpenSandbox-Egress-Credential` JWT offline
    against the Ed25519 public key identified by `kid`.

    The endpoint is public by design — it serves public keys only (the
    OIDC `/.well-known/jwks.json` posture) and requires no API key; abuse
    is bounded by local `kid` grammar validation and a per-window lookup
    budget. `kid` is a platform identifier: a fast-sandbox pool kid
    (`<pool>:v<version>`) or a sidecar sandbox's sandboxId.
    Off-grammar and unknown kids answer 404 without a backend read.

    Args:
        kid (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | EgressSigningJwk]
    """

    kwargs = _get_kwargs(
        kid=kid,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    kid: str,
    *,
    client: AuthenticatedClient | Client,
) -> Any | EgressSigningJwk | None:
    """Publish one credential-vault resolver caller-authentication public key

     Credential-vault resolver caller authentication: credential
    resolvers verify the `OpenSandbox-Egress-Credential` JWT offline
    against the Ed25519 public key identified by `kid`.

    The endpoint is public by design — it serves public keys only (the
    OIDC `/.well-known/jwks.json` posture) and requires no API key; abuse
    is bounded by local `kid` grammar validation and a per-window lookup
    budget. `kid` is a platform identifier: a fast-sandbox pool kid
    (`<pool>:v<version>`) or a sidecar sandbox's sandboxId.
    Off-grammar and unknown kids answer 404 without a backend read.

    Args:
        kid (str):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | EgressSigningJwk
    """

    return (
        await asyncio_detailed(
            kid=kid,
            client=client,
        )
    ).parsed
