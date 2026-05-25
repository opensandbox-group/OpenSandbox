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
Adapter layer for OpenSandbox SDK.

Implements the service protocols using HTTP API calls.
"""

from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.adapters.egress_adapter import EgressAdapter
from opensandbox.adapters.factory import AdapterFactory
from opensandbox.adapters.filesystem_adapter import FilesystemAdapter
from opensandbox.adapters.health_adapter import HealthAdapter
from opensandbox.adapters.metrics_adapter import MetricsAdapter
from opensandbox.adapters.sandboxes_adapter import SandboxesAdapter

__all__ = [
    "AdapterFactory",
    "SandboxesAdapter",
    "FilesystemAdapter",
    "CommandsAdapter",
    "EgressAdapter",
    "HealthAdapter",
    "MetricsAdapter",
]
