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

from contextvars import ContextVar
from typing import Optional, Tuple

from opensandbox_server.tenants.models import TenantEntry

_current_tenant: ContextVar[Optional[TenantEntry]] = ContextVar("current_tenant", default=None)

# Namespace resolved for a sandbox id in the current execution flow only.
_resolved_sandbox_ns: ContextVar[Optional[Tuple[str, str]]] = ContextVar(
    "resolved_sandbox_ns", default=None
)

# Namespace as observed by ingress at observed_at (renew-intent payload).
# First-hand but time-bounded: by processing time the sandbox may have moved,
# so the lookup must still verify a workload exists there before using it.
_observed_namespace: ContextVar[Optional[str]] = ContextVar("observed_namespace", default=None)


def get_current_tenant() -> Optional[TenantEntry]:
    return _current_tenant.get()


def set_current_tenant(tenant: Optional[TenantEntry]) -> None:
    _current_tenant.set(tenant)


def get_resolved_sandbox_ns(sandbox_id: str) -> Optional[str]:
    """Return the namespace resolved earlier in this context for this id, if any."""
    hit = _resolved_sandbox_ns.get()
    if hit is not None and hit[0] == sandbox_id:
        return hit[1]
    return None


def remember_resolved_sandbox_ns(sandbox_id: str, namespace: str) -> None:
    """Memoize a namespace resolution for the rest of the current context only."""
    _resolved_sandbox_ns.set((sandbox_id, namespace))


def reset_resolved_sandbox_ns() -> None:
    """Drop the memoized resolution; the next lookup starts from scratch."""
    _resolved_sandbox_ns.set(None)


def get_observed_namespace() -> Optional[str]:
    """Return the namespace ingress observed for the current attempt, if any."""
    return _observed_namespace.get()


def set_observed_namespace(namespace: Optional[str]) -> None:
    """Set (or clear) the ingress-observed namespace for the current attempt."""
    _observed_namespace.set(namespace)
