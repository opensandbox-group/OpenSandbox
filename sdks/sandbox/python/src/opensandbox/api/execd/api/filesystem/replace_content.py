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
from ...models.error_response import ErrorResponse
from ...models.replace_content_body import ReplaceContentBody
from ...models.replace_content_response_200 import ReplaceContentResponse200
from ...types import UNSET, Response, Unset


def _get_kwargs(
    *,
    body: ReplaceContentBody,
    verbose: bool | Unset = False,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    params: dict[str, Any] = {}

    params["verbose"] = verbose

    params = {k: v for k, v in params.items() if v is not UNSET and v is not None}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/files/replace",
        "params": params,
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> ErrorResponse | ReplaceContentResponse200 | None:
    if response.status_code == 200:
        response_200 = ReplaceContentResponse200.from_dict(response.json())

        return response_200

    if response.status_code == 400:
        response_400 = ErrorResponse.from_dict(response.json())

        return response_400

    if response.status_code == 500:
        response_500 = ErrorResponse.from_dict(response.json())

        return response_500

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[ErrorResponse | ReplaceContentResponse200]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: ReplaceContentBody,
    verbose: bool | Unset = False,
) -> Response[ErrorResponse | ReplaceContentResponse200]:
    """Replace file content

     Performs text replacement in one or multiple files. Replaces all occurrences
    of the old string with the new string (similar to strings.ReplaceAll).
    Preserves file permissions. Useful for batch text substitution across files.

    When `verbose=true` is set, the response includes per-file replacement counts.
    Without this parameter, the response body is empty (backward-compatible behavior).

    Args:
        verbose (bool | Unset):  Default: False.
        body (ReplaceContentBody):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | ReplaceContentResponse200]
    """

    kwargs = _get_kwargs(
        body=body,
        verbose=verbose,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    *,
    client: AuthenticatedClient | Client,
    body: ReplaceContentBody,
    verbose: bool | Unset = False,
) -> ErrorResponse | ReplaceContentResponse200 | None:
    """Replace file content

     Performs text replacement in one or multiple files. Replaces all occurrences
    of the old string with the new string (similar to strings.ReplaceAll).
    Preserves file permissions. Useful for batch text substitution across files.

    When `verbose=true` is set, the response includes per-file replacement counts.
    Without this parameter, the response body is empty (backward-compatible behavior).

    Args:
        verbose (bool | Unset):  Default: False.
        body (ReplaceContentBody):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | ReplaceContentResponse200
    """

    return sync_detailed(
        client=client,
        body=body,
        verbose=verbose,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: ReplaceContentBody,
    verbose: bool | Unset = False,
) -> Response[ErrorResponse | ReplaceContentResponse200]:
    """Replace file content

     Performs text replacement in one or multiple files. Replaces all occurrences
    of the old string with the new string (similar to strings.ReplaceAll).
    Preserves file permissions. Useful for batch text substitution across files.

    When `verbose=true` is set, the response includes per-file replacement counts.
    Without this parameter, the response body is empty (backward-compatible behavior).

    Args:
        verbose (bool | Unset):  Default: False.
        body (ReplaceContentBody):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | ReplaceContentResponse200]
    """

    kwargs = _get_kwargs(
        body=body,
        verbose=verbose,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    body: ReplaceContentBody,
    verbose: bool | Unset = False,
) -> ErrorResponse | ReplaceContentResponse200 | None:
    """Replace file content

     Performs text replacement in one or multiple files. Replaces all occurrences
    of the old string with the new string (similar to strings.ReplaceAll).
    Preserves file permissions. Useful for batch text substitution across files.

    When `verbose=true` is set, the response includes per-file replacement counts.
    Without this parameter, the response body is empty (backward-compatible behavior).

    Args:
        verbose (bool | Unset):  Default: False.
        body (ReplaceContentBody):

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | ReplaceContentResponse200
    """

    return (
        await asyncio_detailed(
            client=client,
            body=body,
            verbose=verbose,
        )
    ).parsed
