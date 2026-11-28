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

from typing import Any, Optional

from opensandbox_server.api.schema import Endpoint
from opensandbox_server.services.constants import (
    DEFAULT_EXECD_PORT,
    EXECD_ACCESS_TOKEN_ENV,
    SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY,
    SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY,
)
from opensandbox_server.services.endpoint_auth import (
    build_egress_auth_headers,
    build_secure_access_headers,
    merge_endpoint_headers,
)
from opensandbox_server.services.execd_auth import (
    HASH_PREFIX,
    ExecdTokenManager,
    build_execd_auth_headers,
)


def _get_egress_auth_token(workload: Any) -> Optional[str]:
    return _get_annotation(workload, SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY)


def _get_secure_access_token(workload: Any) -> Optional[str]:
    return _get_annotation(workload, SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY)


def _get_annotation(workload: Any, key: str) -> Optional[str]:
    if isinstance(workload, dict):
        metadata = workload.get("metadata", {})
        annotations = metadata.get("annotations", {}) or {}
        return annotations.get(key)

    metadata = getattr(workload, "metadata", None)
    annotations = getattr(metadata, "annotations", None) or {}
    if isinstance(annotations, dict):
        return annotations.get(key)
    return None


def _attach_egress_auth_headers(endpoint: Endpoint, workload: Any, port: int) -> None:
    if port != 18080:
        return
    token = _get_egress_auth_token(workload)
    if not token:
        return
    endpoint.headers = merge_endpoint_headers(
        endpoint.headers,
        build_egress_auth_headers(token),
    )


def _attach_secure_access_headers(endpoint: Endpoint, workload: Any) -> None:
    token = _get_secure_access_token(workload)
    if not token:
        return
    endpoint.headers = merge_endpoint_headers(
        endpoint.headers,
        build_secure_access_headers(token),
    )


def _extract_env_value(env_list: Any, name: str) -> Optional[str]:
    if not isinstance(env_list, list):
        return None
    for item in env_list:
        if isinstance(item, dict) and item.get("name") == name:
            val = item.get("value")
            return val if isinstance(val, str) and val else None
    return None


def _extract_execd_access_token_env(workload: Any) -> Optional[str]:
    """Extract EXECD_ACCESS_TOKEN from BatchSandbox (normal/pool) or AgentSandbox CR."""
    if not isinstance(workload, dict):
        return None
    spec = workload.get("spec")
    if not isinstance(spec, dict):
        return None

    # Pool mode: spec.taskTemplate.spec.process.env
    task_env = (
        (spec.get("taskTemplate") or {})
        .get("spec", {})
        .get("process", {})
        .get("env")
    )
    if (val := _extract_env_value(task_env, EXECD_ACCESS_TOKEN_ENV)) is not None:
        return val

    # BatchSandbox normal mode (spec.template.spec.containers) &
    # AgentSandbox mode (spec.podTemplate.spec.containers)
    for template_key in ("template", "podTemplate"):
        containers = (spec.get(template_key) or {}).get("spec", {}).get("containers")
        if isinstance(containers, list):
            for container in containers:
                if isinstance(container, dict):
                    if (val := _extract_env_value(container.get("env"), EXECD_ACCESS_TOKEN_ENV)) is not None:
                        return val
    return None


def _attach_execd_auth_headers(
    endpoint: Endpoint,
    workload: Any,
    port: int,
    manager: Optional[ExecdTokenManager],
) -> None:
    if port != DEFAULT_EXECD_PORT:
        return
    env_value = _extract_execd_access_token_env(workload)
    if not env_value:
        return
    if env_value.strip().startswith(HASH_PREFIX):
        if manager is None:
            raise ValueError(
                "Workload carries a hashed EXECD_ACCESS_TOKEN verifier but execd.auth_signing_key_file is not configured"
            )
        raw_token = manager.resolve_raw_token(env_value)
    else:
        raw_token = env_value
    endpoint.headers = merge_endpoint_headers(
        endpoint.headers,
        build_execd_auth_headers(raw_token),
    )
