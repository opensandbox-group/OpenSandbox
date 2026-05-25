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
Synchronous service interfaces (Protocols) for the sync SDK.
"""

from opensandbox.sync.services.command import CommandsSync
from opensandbox.sync.services.diagnostics import DiagnosticsSync
from opensandbox.sync.services.egress import EgressSync
from opensandbox.sync.services.filesystem import FilesystemSync
from opensandbox.sync.services.health import HealthSync
from opensandbox.sync.services.metrics import MetricsSync
from opensandbox.sync.services.sandbox import SandboxesSync

__all__ = [
    "CommandsSync",
    "DiagnosticsSync",
    "EgressSync",
    "FilesystemSync",
    "HealthSync",
    "MetricsSync",
    "SandboxesSync",
]
