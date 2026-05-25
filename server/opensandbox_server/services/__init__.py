# Copyright 2025 Alibaba Group Holding Ltd.
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

"""Sandbox service implementations."""

from opensandbox_server.services.docker import DockerSandboxService
from opensandbox_server.services.extension_service import ExtensionService, require_extension_service
from opensandbox_server.services.k8s.kubernetes_service import KubernetesSandboxService
from opensandbox_server.services.factory import create_sandbox_service
from opensandbox_server.services.sandbox_service import SandboxService
from opensandbox_server.services.snapshot_models import (
    SnapshotRecord,
    SnapshotRestoreConfig,
    SnapshotState,
    SnapshotStatusRecord,
)
from opensandbox_server.services.snapshot_repository import (
    SnapshotListQuery,
    SnapshotListResult,
    SnapshotRepository,
)
from opensandbox_server.services.snapshot_runtime import (
    NoopSnapshotRuntime,
    SnapshotRuntime,
    SnapshotRuntimeStatus,
)
from opensandbox_server.services.snapshot_service import (
    PersistedSnapshotService,
    SnapshotService,
    create_snapshot_service,
)

__all__ = [
    "SandboxService",
    "SnapshotState",
    "SnapshotRestoreConfig",
    "SnapshotStatusRecord",
    "SnapshotRecord",
    "SnapshotRepository",
    "SnapshotRuntime",
    "SnapshotRuntimeStatus",
    "NoopSnapshotRuntime",
    "SnapshotListQuery",
    "SnapshotListResult",
    "SnapshotService",
    "PersistedSnapshotService",
    "create_snapshot_service",
    "ExtensionService",
    "require_extension_service",
    "DockerSandboxService",
    "KubernetesSandboxService",
    "create_sandbox_service",
]
