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
from ...models.error_response import ErrorResponse
from ...models.identity_replace_content_body import IdentityReplaceContentBody
from ...models.identity_replace_content_response_200 import IdentityReplaceContentResponse200
from ...types import UNSET, Response, Unset


def _get_kwargs(
    uid: int,
    gid: int,
    *,
    body: IdentityReplaceContentBody,
    verbose: bool | Unset = False,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    params: dict[str, Any] = {}

    params["verbose"] = verbose

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/v1/filesystem/{uid}/{gid}/files/replace".format(
            uid=quote(str(uid), safe=""),
            gid=quote(str(gid), safe=""),
        ),
        "params": params,
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Any | ErrorResponse | IdentityReplaceContentResponse200 | None:
    if response.status_code == 200:
        if not response.content:
            return cast(Any, None)
        response_200 = IdentityReplaceContentResponse200.from_dict(response.json())

        return response_200

    if response.status_code == 400:
        response_400 = ErrorResponse.from_dict(response.json())

        return response_400

    if response.status_code == 500:
        response_500 = ErrorResponse.from_dict(response.json())

        return response_500

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
) -> Response[Any | ErrorResponse | IdentityReplaceContentResponse200]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    uid: int,
    gid: int,
    *,
    client: AuthenticatedClient | Client,
    body: IdentityReplaceContentBody,
    verbose: bool | Unset = False,
) -> Response[Any | ErrorResponse | IdentityReplaceContentResponse200]:
    """Replace file content

     Performs text replacement in one or multiple files. Replaces all occurrences
    of the old string with the new string (similar to strings.ReplaceAll).
    Preserves file permissions. Useful for batch text substitution across files.

    When `verbose=true` is set, the response includes per-file replacement counts.
    Without this parameter, the response body is empty (backward-compatible behavior).

    Args:
        uid (int):
        gid (int):
        verbose (bool | Unset):  Default: False.
        body (IdentityReplaceContentBody):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | ErrorResponse | IdentityReplaceContentResponse200]
    """

    kwargs = _get_kwargs(
        uid=uid,
        gid=gid,
        body=body,
        verbose=verbose,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    uid: int,
    gid: int,
    *,
    client: AuthenticatedClient | Client,
    body: IdentityReplaceContentBody,
    verbose: bool | Unset = False,
) -> Any | ErrorResponse | IdentityReplaceContentResponse200 | None:
    """Replace file content

     Performs text replacement in one or multiple files. Replaces all occurrences
    of the old string with the new string (similar to strings.ReplaceAll).
    Preserves file permissions. Useful for batch text substitution across files.

    When `verbose=true` is set, the response includes per-file replacement counts.
    Without this parameter, the response body is empty (backward-compatible behavior).

    Args:
        uid (int):
        gid (int):
        verbose (bool | Unset):  Default: False.
        body (IdentityReplaceContentBody):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | ErrorResponse | IdentityReplaceContentResponse200
    """

    return sync_detailed(
        uid=uid,
        gid=gid,
        client=client,
        body=body,
        verbose=verbose,
    ).parsed


async def asyncio_detailed(
    uid: int,
    gid: int,
    *,
    client: AuthenticatedClient | Client,
    body: IdentityReplaceContentBody,
    verbose: bool | Unset = False,
) -> Response[Any | ErrorResponse | IdentityReplaceContentResponse200]:
    """Replace file content

     Performs text replacement in one or multiple files. Replaces all occurrences
    of the old string with the new string (similar to strings.ReplaceAll).
    Preserves file permissions. Useful for batch text substitution across files.

    When `verbose=true` is set, the response includes per-file replacement counts.
    Without this parameter, the response body is empty (backward-compatible behavior).

    Args:
        uid (int):
        gid (int):
        verbose (bool | Unset):  Default: False.
        body (IdentityReplaceContentBody):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | ErrorResponse | IdentityReplaceContentResponse200]
    """

    kwargs = _get_kwargs(
        uid=uid,
        gid=gid,
        body=body,
        verbose=verbose,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    uid: int,
    gid: int,
    *,
    client: AuthenticatedClient | Client,
    body: IdentityReplaceContentBody,
    verbose: bool | Unset = False,
) -> Any | ErrorResponse | IdentityReplaceContentResponse200 | None:
    """Replace file content

     Performs text replacement in one or multiple files. Replaces all occurrences
    of the old string with the new string (similar to strings.ReplaceAll).
    Preserves file permissions. Useful for batch text substitution across files.

    When `verbose=true` is set, the response includes per-file replacement counts.
    Without this parameter, the response body is empty (backward-compatible behavior).

    Args:
        uid (int):
        gid (int):
        verbose (bool | Unset):  Default: False.
        body (IdentityReplaceContentBody):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | ErrorResponse | IdentityReplaceContentResponse200
    """

    return (
        await asyncio_detailed(
            uid=uid,
            gid=gid,
            client=client,
            body=body,
            verbose=verbose,
        )
    ).parsed
