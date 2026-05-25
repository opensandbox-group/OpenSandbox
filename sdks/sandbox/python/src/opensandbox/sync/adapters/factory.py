#
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
#
"""
Synchronous service factory for creating sync adapter instances.
"""

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.models.sandboxes import SandboxEndpoint
from opensandbox.sync.adapters.command_adapter import CommandsAdapterSync
from opensandbox.sync.adapters.diagnostics_adapter import DiagnosticsAdapterSync
from opensandbox.sync.adapters.egress_adapter import EgressAdapterSync
from opensandbox.sync.adapters.filesystem_adapter import FilesystemAdapterSync
from opensandbox.sync.adapters.health_adapter import HealthAdapterSync
from opensandbox.sync.adapters.metrics_adapter import MetricsAdapterSync
from opensandbox.sync.adapters.sandboxes_adapter import SandboxesAdapterSync
from opensandbox.sync.services import (
    CommandsSync,
    DiagnosticsSync,
    EgressSync,
    FilesystemSync,
    HealthSync,
    MetricsSync,
    SandboxesSync,
)


class AdapterFactorySync:
    def __init__(self, connection_config: ConnectionConfigSync) -> None:
        self.connection_config = connection_config

    def create_sandbox_service(self) -> SandboxesSync:
        return SandboxesAdapterSync(self.connection_config)

    def create_diagnostics_service(self) -> DiagnosticsSync:
        return DiagnosticsAdapterSync(self.connection_config)

    def create_filesystem_service(self, endpoint: SandboxEndpoint) -> FilesystemSync:
        return FilesystemAdapterSync(self.connection_config, endpoint)

    def create_command_service(self, endpoint: SandboxEndpoint) -> CommandsSync:
        return CommandsAdapterSync(self.connection_config, endpoint)

    def create_egress_service(self, endpoint: SandboxEndpoint) -> EgressSync:
        return EgressAdapterSync(self.connection_config, endpoint)

    def create_health_service(self, endpoint: SandboxEndpoint) -> HealthSync:
        return HealthAdapterSync(self.connection_config, endpoint)

    def create_metrics_service(self, endpoint: SandboxEndpoint) -> MetricsSync:
        return MetricsAdapterSync(self.connection_config, endpoint)
