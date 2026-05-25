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

from typing import Any, Optional

from opensandbox_server.api.schema import Endpoint
from opensandbox_server.services.constants import (
    SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY,
    SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY,
)
from opensandbox_server.services.endpoint_auth import (
    build_egress_auth_headers,
    build_secure_access_headers,
    merge_endpoint_headers,
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


def _attach_egress_auth_headers(endpoint: Endpoint, workload: Any) -> None:
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
