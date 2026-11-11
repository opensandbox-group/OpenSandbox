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

from __future__ import annotations

from typing import Any

from opensandbox_server.services.constants import SANDBOX_TENANT_LABEL
from opensandbox_server.services.k8s.error_helpers import (
    _build_sandbox_not_found_error,
    _is_not_found_error,
)
from opensandbox_server.tenants.context import get_current_tenant


def _workload_labels(workload: Any) -> dict[str, str]:
    if isinstance(workload, dict):
        labels = (workload.get("metadata") or {}).get("labels") or {}
    else:
        metadata = getattr(workload, "metadata", None)
        labels = getattr(metadata, "labels", None) if metadata is not None else None
        labels = labels or {}
    if not isinstance(labels, dict):
        return {}
    return {str(k): str(v) for k, v in labels.items()}


def _enforce_tenant_ownership(workload: Any, sandbox_id: str) -> None:
    """Hide another tenant's sandbox as 404 when API keys share a namespace."""
    tenant = get_current_tenant()
    if tenant is None:
        return
    owner = _workload_labels(workload).get(SANDBOX_TENANT_LABEL)
    if owner is None or owner == tenant.name:
        return
    raise _build_sandbox_not_found_error(sandbox_id)


def _get_owned_workload_or_404(
    workload_provider: Any,
    namespace: str,
    sandbox_id: str,
) -> Any:
    workload = _get_workload_or_404(workload_provider, namespace, sandbox_id)
    _enforce_tenant_ownership(workload, sandbox_id)
    return workload


def _get_workload_or_404(
    workload_provider: Any,
    namespace: str,
    sandbox_id: str,
) -> Any:
    workload = workload_provider.get_workload(
        sandbox_id=sandbox_id,
        namespace=namespace,
    )
    if not workload:
        raise _build_sandbox_not_found_error(sandbox_id)
    return workload


def _delete_workload_or_404(
    workload_provider: Any,
    namespace: str,
    sandbox_id: str,
) -> None:
    try:
        workload_provider.delete_workload(
            sandbox_id=sandbox_id,
            namespace=namespace,
        )
    except Exception as exc:
        if _is_not_found_error(exc):
            raise _build_sandbox_not_found_error(sandbox_id) from exc
        raise