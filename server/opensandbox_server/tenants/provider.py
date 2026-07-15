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

from __future__ import annotations

from typing import Callable, List, Optional, Protocol, runtime_checkable

from opensandbox_server.tenants.models import TenantEntry


@runtime_checkable
class TenantProvider(Protocol):
    """Abstraction for tenant resolution.

    Auth middleware depends only on this interface, not on any specific
    config source. Implementations may be backed by a local file, an
    HTTP endpoint, a Kubernetes Secret, or any other tenant store.
    """

    def lookup(self, api_key: str) -> Optional[TenantEntry]:
        """Resolve an API key to a tenant entry.

        Returns None if the key is not recognized.
        Raises TenantProviderUnavailable if the provider cannot serve lookups.
        """
        ...

    def list_tenants(self) -> List[TenantEntry]:
        """Return all known tenant entries (used for startup validation)."""
        ...

    def ready(self) -> bool:
        """True once the provider has loaded initial state and can serve lookups."""
        ...

    def start(self) -> None:
        """Start background resources (watchers, pollers). Called once at server startup."""
        ...

    def close(self) -> None:
        """Release resources (threads, connections). Called on server shutdown."""
        ...

    def on_reload(self, callback: Callable[[List[TenantEntry]], None]) -> None:
        """Register a callback invoked when tenant data changes.

        The callback receives the new full list of tenant entries.
        Not all providers support change notification; those that don't
        may silently ignore this call.
        """
        ...


class TenantProviderUnavailable(Exception):
    """Raised when a provider cannot serve lookups (e.g. remote unreachable + cache expired)."""
