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
from typing import Any, cast

import httpx

from ... import errors
from ...client import AuthenticatedClient, Client
from ...models.error_response import ErrorResponse
from ...models.metrics_event import MetricsEvent
from ...types import Response


def _get_kwargs(
    *,
    body: MetricsEvent,
) -> dict[str, Any]:
    headers: dict[str, Any] = {}

    _kwargs: dict[str, Any] = {
        "method": "post",
        "url": "/metrics/events",
    }

    _kwargs["json"] = body.to_dict()

    headers["Content-Type"] = "application/json"

    _kwargs["headers"] = headers
    return _kwargs


def _parse_response(*, client: AuthenticatedClient | Client, response: httpx.Response) -> Any | ErrorResponse | None:
    if response.status_code == 204:
        response_204 = cast(Any, None)
        return response_204

    if response.status_code == 400:
        response_400 = ErrorResponse.from_dict(response.json())

        return response_400

    if response.status_code == 401:
        response_401 = ErrorResponse.from_dict(response.json())

        return response_401

    if response.status_code == 500:
        response_500 = ErrorResponse.from_dict(response.json())

        return response_500

    if client.raise_on_unexpected_status:
        raise errors.UnexpectedStatus(response.status_code, response.content)
    else:
        return None


def _build_response(*, client: AuthenticatedClient | Client, response: httpx.Response) -> Response[Any | ErrorResponse]:
    return Response(
        status_code=HTTPStatus(response.status_code),
        content=response.content,
        headers=response.headers,
        parsed=_parse_response(client=client, response=response),
    )


def sync_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: MetricsEvent,
) -> Response[Any | ErrorResponse]:
    """Report an SDK metrics event

     Accepts best-effort telemetry from SDKs (Phase 1: sandbox creation latency).

    SDKs SHOULD fire-and-forget this call after `create` + readiness complete
    (or after a failed creation attempt). Failures to report MUST NOT affect
    sandbox usability. The server accepts events even when OpenTelemetry
    export is disabled (noop recording).

    SDK language and version are taken from the HTTP `User-Agent` header
    (for example `OpenSandbox-Python-SDK/0.1.14`), not from the JSON body.

    Args:
        body (MetricsEvent): SDK-reported metrics event. Phase 1 covers sandbox creation latency
            from
            the start of create until readiness succeeds or creation fails.

            SDK language and package version are identified via the request
            `User-Agent` header (for example `OpenSandbox-Go-SDK/0.1.0`), not body fields.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | ErrorResponse]
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
    body: MetricsEvent,
) -> Any | ErrorResponse | None:
    """Report an SDK metrics event

     Accepts best-effort telemetry from SDKs (Phase 1: sandbox creation latency).

    SDKs SHOULD fire-and-forget this call after `create` + readiness complete
    (or after a failed creation attempt). Failures to report MUST NOT affect
    sandbox usability. The server accepts events even when OpenTelemetry
    export is disabled (noop recording).

    SDK language and version are taken from the HTTP `User-Agent` header
    (for example `OpenSandbox-Python-SDK/0.1.14`), not from the JSON body.

    Args:
        body (MetricsEvent): SDK-reported metrics event. Phase 1 covers sandbox creation latency
            from
            the start of create until readiness succeeds or creation fails.

            SDK language and package version are identified via the request
            `User-Agent` header (for example `OpenSandbox-Go-SDK/0.1.0`), not body fields.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | ErrorResponse
    """

    return sync_detailed(
        client=client,
        body=body,
    ).parsed


async def asyncio_detailed(
    *,
    client: AuthenticatedClient | Client,
    body: MetricsEvent,
) -> Response[Any | ErrorResponse]:
    """Report an SDK metrics event

     Accepts best-effort telemetry from SDKs (Phase 1: sandbox creation latency).

    SDKs SHOULD fire-and-forget this call after `create` + readiness complete
    (or after a failed creation attempt). Failures to report MUST NOT affect
    sandbox usability. The server accepts events even when OpenTelemetry
    export is disabled (noop recording).

    SDK language and version are taken from the HTTP `User-Agent` header
    (for example `OpenSandbox-Python-SDK/0.1.14`), not from the JSON body.

    Args:
        body (MetricsEvent): SDK-reported metrics event. Phase 1 covers sandbox creation latency
            from
            the start of create until readiness succeeds or creation fails.

            SDK language and package version are identified via the request
            `User-Agent` header (for example `OpenSandbox-Go-SDK/0.1.0`), not body fields.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Response[Any | ErrorResponse]
    """

    kwargs = _get_kwargs(
        body=body,
    )

    response = await client.get_async_httpx_client().request(**kwargs)

    return _build_response(client=client, response=response)


async def asyncio(
    *,
    client: AuthenticatedClient | Client,
    body: MetricsEvent,
) -> Any | ErrorResponse | None:
    """Report an SDK metrics event

     Accepts best-effort telemetry from SDKs (Phase 1: sandbox creation latency).

    SDKs SHOULD fire-and-forget this call after `create` + readiness complete
    (or after a failed creation attempt). Failures to report MUST NOT affect
    sandbox usability. The server accepts events even when OpenTelemetry
    export is disabled (noop recording).

    SDK language and version are taken from the HTTP `User-Agent` header
    (for example `OpenSandbox-Python-SDK/0.1.14`), not from the JSON body.

    Args:
        body (MetricsEvent): SDK-reported metrics event. Phase 1 covers sandbox creation latency
            from
            the start of create until readiness succeeds or creation fails.

            SDK language and package version are identified via the request
            `User-Agent` header (for example `OpenSandbox-Go-SDK/0.1.0`), not body fields.

    Raises:
        errors.UnexpectedStatus: If the server returns an undocumented status code and Client.raise_on_unexpected_status is True.
        httpx.TimeoutException: If the request takes longer than Client.timeout.

    Returns:
        Any | ErrorResponse
    """

    return (
        await asyncio_detailed(
            client=client,
            body=body,
        )
    ).parsed
