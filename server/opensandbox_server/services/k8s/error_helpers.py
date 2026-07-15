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

from fastapi import HTTPException, status

from opensandbox_server.services.constants import SandboxErrorCodes


def _build_sandbox_not_found_error(sandbox_id: str) -> HTTPException:
    return HTTPException(
        status_code=status.HTTP_404_NOT_FOUND,
        detail={
            "code": SandboxErrorCodes.K8S_SANDBOX_NOT_FOUND,
            "message": f"Sandbox '{sandbox_id}' not found",
        },
    )


def _build_k8s_api_error(action: str, exc: Exception) -> HTTPException:
    return HTTPException(
        status_code=status.HTTP_500_INTERNAL_SERVER_ERROR,
        detail={
            "code": SandboxErrorCodes.K8S_API_ERROR,
            "message": f"Failed to {action}: {str(exc)}",
        },
    )


def _is_not_found_error(exc: Exception) -> bool:
    return "not found" in str(exc).lower()