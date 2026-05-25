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

from dataclasses import dataclass
from datetime import datetime
from typing import Callable, Dict, Optional

from opensandbox_server.api.schema import CreateSandboxRequest
from opensandbox_server.config import AppConfig, EGRESS_MODE_DNS
from opensandbox_server.services.constants import (
    SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY,
    SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY,
    SANDBOX_ID_LABEL,
    SANDBOX_MANUAL_CLEANUP_LABEL,
    SANDBOX_SNAPSHOT_ID_LABEL,
)
from opensandbox_server.services.validators import calculate_expiration_or_raise


@dataclass
class _CreateWorkloadContext:
    labels: Dict[str, str]
    annotations: Dict[str, str]
    expires_at: Optional[datetime]
    resource_limits: Dict[str, str]
    egress_mode: str
    egress_image: Optional[str]
    egress_auth_token: Optional[str]
    secure_access_token: Optional[str]


def _build_create_workload_context(
    app_config: AppConfig,
    request: CreateSandboxRequest,
    sandbox_id: str,
    created_at: datetime,
    egress_token_factory: Callable[[], str],
    secure_access_token_factory: Callable[[], str],
) -> _CreateWorkloadContext:
    expires_at = None
    if request.timeout is not None:
        expires_at = calculate_expiration_or_raise(created_at, request.timeout)

    labels: Dict[str, str] = {SANDBOX_ID_LABEL: sandbox_id}
    if expires_at is None:
        labels[SANDBOX_MANUAL_CLEANUP_LABEL] = "true"
    if request.snapshot_id:
        labels[SANDBOX_SNAPSHOT_ID_LABEL] = request.snapshot_id
    if request.metadata:
        labels.update(request.metadata)

    annotations: Dict[str, str] = {}
    secure_access_token = None
    if request.secure_access:
        secure_access_token = secure_access_token_factory()
        annotations[SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY] = secure_access_token

    egress_mode = app_config.egress.mode if app_config.egress else EGRESS_MODE_DNS
    egress_image = None
    egress_auth_token = None
    if request.network_policy:
        egress_image = app_config.egress.image if app_config.egress else None
        egress_auth_token = egress_token_factory()
        annotations[SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY] = egress_auth_token

    resource_limits = {}
    if request.resource_limits and request.resource_limits.root:
        resource_limits = request.resource_limits.root

    return _CreateWorkloadContext(
        labels=labels,
        annotations=annotations,
        expires_at=expires_at,
        resource_limits=resource_limits,
        egress_mode=egress_mode,
        egress_image=egress_image,
        egress_auth_token=egress_auth_token,
        secure_access_token=secure_access_token,
    )
