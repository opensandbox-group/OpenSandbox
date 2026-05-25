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
OpenSandbox data models.

Core Pydantic models for sandbox operations.
"""

from opensandbox.models.diagnostics import DiagnosticContent
from opensandbox.models.execd import (
    CommandLogs,
    CommandStatus,
    Execution,
    ExecutionComplete,
    ExecutionError,
    ExecutionInit,
    ExecutionLogs,
    ExecutionResult,
    OutputMessage,
)
from opensandbox.models.filesystem import (
    ContentReplaceEntry,
    EntryInfo,
    MoveEntry,
    SearchEntry,
    SetPermissionEntry,
    WriteEntry,
)
from opensandbox.models.sandboxes import (
    PVC,
    Host,
    NetworkPolicy,
    NetworkRule,
    PagedSandboxInfos,
    PaginationInfo,
    PlatformSpec,
    SandboxCreateResponse,
    SandboxEndpoint,
    SandboxFilter,
    SandboxImageAuth,
    SandboxImageSpec,
    SandboxInfo,
    SandboxMetrics,
    SandboxState,
    SandboxStatus,
    Volume,
)

__all__ = [
    # Execution models
    "DiagnosticContent",
    "Execution",
    "ExecutionLogs",
    "OutputMessage",
    "ExecutionResult",
    "ExecutionError",
    "ExecutionComplete",
    "ExecutionInit",
    "CommandStatus",
    "CommandLogs",
    # Filesystem models
    "EntryInfo",
    "WriteEntry",
    "MoveEntry",
    "SetPermissionEntry",
    "ContentReplaceEntry",
    "SearchEntry",
    # Sandbox models
    "SandboxInfo",
    "SandboxStatus",
    "SandboxState",
    "NetworkPolicy",
    "NetworkRule",
    "PlatformSpec",
    "SandboxCreateResponse",
    "SandboxEndpoint",
    "SandboxImageSpec",
    "SandboxImageAuth",
    "SandboxFilter",
    "SandboxMetrics",
    "PagedSandboxInfos",
    "PaginationInfo",
    # Volume models
    "Volume",
    "Host",
    "PVC",
]
