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

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error_response import ErrorResponse
from ...models.run_command_request import RunCommandRequest
from ...models.server_stream_event import ServerStreamEvent
from ...types import UNSET, Response, Unset


def _get_kwargs(
    *,
    body: RunCommandRequest,
    x_request_id: str | Unset = UNSET,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/command",
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    if not isinstance(x_request_id, Unset):
        headers["X-Request-Id"] = x_request_id

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> ErrorResponse | ServerStreamEvent | None:
    if response.status_code == 200:
        response_200 = ServerStreamEvent.from_dict(response.text)

        return response_200

    if response.status_code == 400:
        try:
            response_400 = ErrorResponse.from_dict(response.json())
        except (ValueError, TypeError, KeyError):
            return None

        return response_400

    if response.status_code == 500:
        try:
            response_500 = ErrorResponse.from_dict(response.json())
        except (ValueError, TypeError, KeyError):
            return None

        return response_500

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(
    *, client: AuthenticatedClient | Client, response: httpx.Response
) -> Response[ErrorResponse | ServerStreamEvent]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: RunCommandRequest,
    x_request_id: str | Unset = UNSET,
) -> Response[ErrorResponse | ServerStreamEvent]:
    r"""Execute shell command or native argv

     Executes shell text (`command`) or native arguments (`argv`) and streams output using SSE; supply
    exactly one input mode.
    The command can run in foreground or background mode. The response includes stdout, stderr,
    execution status, and completion events.
    In foreground mode, stdout and stderr event text preserves the original line endings
    (`\n`, `\r`, or `\r\n`). Concatenate text from each stream's events to recover its output;
    the final event may contain an unterminated fragment.
    Optionally specify `timeout` (milliseconds) to enforce a maximum runtime; the server will
    terminate the process when the timeout is reached. You can also pass `uid`/`gid` to run
    with specific user/group IDs, and `envs` to inject environment variables.

    Args:
        body (RunCommandRequest): Execute exactly one of command (shell text) or argv (native
            arguments).
        x_request_id (str | Unset): Optional end-to-end request id (at most 128 characters
            from [A-Za-z0-9._-]). Exported as SANDBOX_REQUEST_ID for the command and its
            descendants; ids that are oversized or use other characters are dropped.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | ServerStreamEvent]
    """

    kwargs = _get_kwargs(
        body=body,
        x_request_id=x_request_id,
    )

    response = client.get_httpx_client().request(
        **kwargs,
    )

    return _build_response(client=client, response=response)


def sync(
    *,
    client: AuthenticatedClient | Client,
    body: RunCommandRequest,
    x_request_id: str | Unset = UNSET,
) -> ErrorResponse | ServerStreamEvent | None:
    r"""Execute shell command or native argv

     Executes shell text (`command`) or native arguments (`argv`) and streams output using SSE; supply
    exactly one input mode.
    The command can run in foreground or background mode. The response includes stdout, stderr,
    execution status, and completion events.
    In foreground mode, stdout and stderr event text preserves the original line endings
    (`\n`, `\r`, or `\r\n`). Concatenate text from each stream's events to recover its output;
    the final event may contain an unterminated fragment.
    Optionally specify `timeout` (milliseconds) to enforce a maximum runtime; the server will
    terminate the process when the timeout is reached. You can also pass `uid`/`gid` to run
    with specific user/group IDs, and `envs` to inject environment variables.

    Args:
        body (RunCommandRequest): Execute exactly one of command (shell text) or argv (native
            arguments).
        x_request_id (str | Unset): Optional end-to-end request id (at most 128 characters
            from [A-Za-z0-9._-]). Exported as SANDBOX_REQUEST_ID for the command and its
            descendants; ids that are oversized or use other characters are dropped.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | ServerStreamEvent
    """

    return sync_detailed(
        client=client,
        body=body,
        x_request_id=x_request_id,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: RunCommandRequest,
    x_request_id: str | Unset = UNSET,
) -> Response[ErrorResponse | ServerStreamEvent]:
    r"""Execute shell command or native argv

     Executes shell text (`command`) or native arguments (`argv`) and streams output using SSE; supply
    exactly one input mode.
    The command can run in foreground or background mode. The response includes stdout, stderr,
    execution status, and completion events.
    In foreground mode, stdout and stderr event text preserves the original line endings
    (`\n`, `\r`, or `\r\n`). Concatenate text from each stream's events to recover its output;
    the final event may contain an unterminated fragment.
    Optionally specify `timeout` (milliseconds) to enforce a maximum runtime; the server will
    terminate the process when the timeout is reached. You can also pass `uid`/`gid` to run
    with specific user/group IDs, and `envs` to inject environment variables.

    Args:
        body (RunCommandRequest): Execute exactly one of command (shell text) or argv (native
            arguments).
        x_request_id (str | Unset): Optional end-to-end request id (at most 128 characters
            from [A-Za-z0-9._-]). Exported as SANDBOX_REQUEST_ID for the command and its
            descendants; ids that are oversized or use other characters are dropped.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[ErrorResponse | ServerStreamEvent]
    """

    kwargs = _get_kwargs(
        body=body,
        x_request_id=x_request_id,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    body: RunCommandRequest,
    x_request_id: str | Unset = UNSET,
) -> ErrorResponse | ServerStreamEvent | None:
    r"""Execute shell command or native argv

     Executes shell text (`command`) or native arguments (`argv`) and streams output using SSE; supply
    exactly one input mode.
    The command can run in foreground or background mode. The response includes stdout, stderr,
    execution status, and completion events.
    In foreground mode, stdout and stderr event text preserves the original line endings
    (`\n`, `\r`, or `\r\n`). Concatenate text from each stream's events to recover its output;
    the final event may contain an unterminated fragment.
    Optionally specify `timeout` (milliseconds) to enforce a maximum runtime; the server will
    terminate the process when the timeout is reached. You can also pass `uid`/`gid` to run
    with specific user/group IDs, and `envs` to inject environment variables.

    Args:
        body (RunCommandRequest): Execute exactly one of command (shell text) or argv (native
            arguments).
        x_request_id (str | Unset): Optional end-to-end request id (at most 128 characters
            from [A-Za-z0-9._-]). Exported as SANDBOX_REQUEST_ID for the command and its
            descendants; ids that are oversized or use other characters are dropped.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        ErrorResponse | ServerStreamEvent
    """

    return (
        await asyncio_detailed(
            client=client,
            body=body,
            x_request_id=x_request_id,
        )
    ).parsed
