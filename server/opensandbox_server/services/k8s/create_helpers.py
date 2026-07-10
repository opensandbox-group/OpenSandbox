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

import logging
from dataclasses import dataclass
from datetime import datetime
from typing import Callable, Dict, Optional

from opensandbox_server.api.schema import CreateSandboxRequest
from opensandbox_server.config import AppConfig, EGRESS_MODE_DNS
from opensandbox_server.services.constants import (
    OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE,
    SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY,
    SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY,
    SANDBOX_ID_LABEL,
    SANDBOX_MANUAL_CLEANUP_LABEL,
    SANDBOX_SNAPSHOT_ID_LABEL,
)
from opensandbox_server.services.helpers import split_egress_env
from opensandbox_server.services.validators import calculate_expiration_or_raise

logger = logging.getLogger(__name__)


@dataclass
class _CreateWorkloadContext:
    labels: Dict[str, str]
    annotations: Dict[str, str]
    expires_at: Optional[datetime]
    resource_limits: Dict[str, str]
    resource_requests: Dict[str, str]
    egress_mode: str
    egress_image: Optional[str]
    egress_auth_token: Optional[str]
    credential_proxy_enabled: bool
    secure_access_token: Optional[str]
    sandbox_env: Dict[str, Optional[str]]
    egress_env: Dict[str, Optional[str]]


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
    credential_proxy_enabled = bool(
        request.credential_proxy and request.credential_proxy.enabled
    )
    if request.network_policy:
        egress_image = app_config.egress.image if app_config.egress else None
        egress_auth_token = egress_token_factory()
        annotations[SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY] = egress_auth_token

    resource_limits = {}
    if request.resource_limits and request.resource_limits.root:
        resource_limits = request.resource_limits.root

    resource_requests = {}
    if request.resource_requests and request.resource_requests.root:
        resource_requests = request.resource_requests.root

    sandbox_env, egress_env = split_egress_env(request.env)

    if credential_proxy_enabled and egress_env.get(OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE):
        raise ValueError(
            f"'{OPENSANDBOX_EGRESS_MITMPROXY_SSL_INSECURE}' cannot be set when credential proxy is enabled"
        )

    if egress_env and not request.network_policy:
        dropped_keys = sorted(egress_env.keys())
        logger.warning(
            "Sandbox %s has OPENSANDBOX_EGRESS_ env vars %s but no networkPolicy; "
            "these variables will be ignored because no egress sidecar is created",
            sandbox_id,
            dropped_keys,
        )
        egress_env = {}

    return _CreateWorkloadContext(
        labels=labels,
        annotations=annotations,
        expires_at=expires_at,
        resource_limits=resource_limits,
        resource_requests=resource_requests,
        egress_mode=egress_mode,
        egress_image=egress_image,
        egress_auth_token=egress_auth_token,
        credential_proxy_enabled=credential_proxy_enabled,
        secure_access_token=secure_access_token,
        sandbox_env=sandbox_env,
        egress_env=egress_env,
    )
