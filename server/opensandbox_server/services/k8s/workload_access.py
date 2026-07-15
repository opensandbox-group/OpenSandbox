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

from typing import Any

from opensandbox_server.services.k8s.error_helpers import (
    _build_sandbox_not_found_error,
    _is_not_found_error,
)


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