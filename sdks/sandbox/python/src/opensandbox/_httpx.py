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
"""httpx helpers shared by handwritten SDK adapters.

These helpers preserve user-provided event hooks while appending the SDK's
request hook that strips protected OpenSandbox headers from cross-origin
requests. The SDK hook must run last so user hooks cannot accidentally re-add
protected headers before httpx sends a redirected request.
"""

from collections.abc import Awaitable, Callable, Mapping, Sequence
from typing import Protocol, TypedDict, TypeVar

import httpx

SyncEventHook = Callable[..., None]
AsyncEventHook = Callable[..., Awaitable[None]]
SyncEventHooks = Mapping[str, Sequence[SyncEventHook]]
AsyncEventHooks = Mapping[str, Sequence[AsyncEventHook]]

_HookT = TypeVar("_HookT")
_PROTECTED_HEADER_PREFIXES = ("OPEN-SANDBOX-", "OPENSANDBOX-")

_DEFAULT_PORTS = {
    "http": 80,
    "https": 443,
}


class _SyncRedirectConfig(Protocol):
    follow_redirects: bool
    event_hooks: dict[str, list[SyncEventHook]]


class _AsyncRedirectConfig(Protocol):
    follow_redirects: bool
    event_hooks: dict[str, list[AsyncEventHook]]


class SyncRedirectClientOptions(TypedDict):
    """Redirect-related options accepted by ``httpx.Client``."""

    follow_redirects: bool
    event_hooks: dict[str, list[SyncEventHook]]


class AsyncRedirectClientOptions(TypedDict):
    """Redirect-related options accepted by ``httpx.AsyncClient``."""

    follow_redirects: bool
    event_hooks: dict[str, list[AsyncEventHook]]


def _origin(url: httpx.URL) -> tuple[str, str | None, int | None]:
    return (url.scheme, url.host, url.port or _DEFAULT_PORTS.get(url.scheme))


def _strip_protected_headers_for_cross_origin_request(
    request: httpx.Request,
    *,
    base_origin: tuple[str, str | None, int | None],
) -> None:
    if _origin(request.url) == base_origin:
        return
    protected_headers = [
        name
        for name in request.headers
        if name.upper().startswith(_PROTECTED_HEADER_PREFIXES)
    ]
    for name in protected_headers:
        del request.headers[name]


def _copy_event_hooks(
    event_hooks: Mapping[str, Sequence[_HookT]] | None,
) -> dict[str, list[_HookT]]:
    if event_hooks is None:
        return {}
    return {event: list(hooks) for event, hooks in event_hooks.items()}


def build_redirect_event_hooks(
    base_url: str,
    event_hooks: SyncEventHooks | None = None,
) -> dict[str, list[SyncEventHook]]:
    """Build sync hooks for SDK clients.

    Existing user hooks are copied and preserved. The SDK request hook is
    appended after user request hooks so protected OpenSandbox headers are
    removed from any request whose origin differs from ``base_url``.
    """
    base_origin = _origin(httpx.URL(base_url))

    def strip_protected_headers(request: httpx.Request) -> None:
        _strip_protected_headers_for_cross_origin_request(
            request, base_origin=base_origin
        )

    hooks = _copy_event_hooks(event_hooks)
    hooks.setdefault("request", []).append(strip_protected_headers)
    return hooks


def build_async_redirect_event_hooks(
    base_url: str,
    event_hooks: AsyncEventHooks | None = None,
) -> dict[str, list[AsyncEventHook]]:
    """Build async hooks for SDK clients.

    Existing user hooks are copied and preserved. The SDK request hook is
    appended after user request hooks so protected OpenSandbox headers are
    removed from any request whose origin differs from ``base_url``.
    """
    base_origin = _origin(httpx.URL(base_url))

    async def strip_protected_headers(request: httpx.Request) -> None:
        _strip_protected_headers_for_cross_origin_request(
            request, base_origin=base_origin
        )

    hooks = _copy_event_hooks(event_hooks)
    hooks.setdefault("request", []).append(strip_protected_headers)
    return hooks


def build_redirect_client_options(
    config: _SyncRedirectConfig,
    base_url: str,
) -> SyncRedirectClientOptions:
    """Build redirect options for a synchronous adapter client."""
    return {
        "follow_redirects": config.follow_redirects,
        "event_hooks": build_redirect_event_hooks(base_url, config.event_hooks),
    }


def build_async_redirect_client_options(
    config: _AsyncRedirectConfig,
    base_url: str,
) -> AsyncRedirectClientOptions:
    """Build redirect options for an asynchronous adapter client."""
    return {
        "follow_redirects": config.follow_redirects,
        "event_hooks": build_async_redirect_event_hooks(base_url, config.event_hooks),
    }
