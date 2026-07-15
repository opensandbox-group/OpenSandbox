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
    ContentReplaceResult,
    DirectoryListEntry,
    EntryInfo,
    MoveEntry,
    SearchEntry,
    SetPermissionEntry,
    WriteEntry,
)
from opensandbox.models.isolated import (
    BindMount,
    CreateIsolatedSessionRequest,
    EnvPassthroughSpec,
    IsolatedCapabilities,
    IsolatedRunOpts,
    IsolatedSessionInfo,
    IsolatedSessionState,
    IsolatedSessionSummary,
    IsolatedWorkspaceSpec,
)
from opensandbox.models.sandboxes import (
    PVC,
    Credential,
    CredentialAuth,
    CredentialBinding,
    CredentialBindingListResponse,
    CredentialBindingMetadata,
    CredentialBindingMutationSet,
    CredentialListResponse,
    CredentialMatch,
    CredentialMetadata,
    CredentialMutationSet,
    CredentialProxyConfig,
    CredentialSubstitution,
    CredentialVaultPatchRequest,
    CredentialVaultState,
    CustomHeaderEntry,
    Host,
    InlineCredentialSource,
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
    # Isolated session models
    "BindMount",
    "CreateIsolatedSessionRequest",
    "EnvPassthroughSpec",
    "IsolatedCapabilities",
    "IsolatedRunOpts",
    "IsolatedSessionInfo",
    "IsolatedSessionState",
    "IsolatedSessionSummary",
    "IsolatedWorkspaceSpec",
    # Filesystem models
    "EntryInfo",
    "WriteEntry",
    "MoveEntry",
    "DirectoryListEntry",
    "SetPermissionEntry",
    "ContentReplaceEntry",
    "ContentReplaceResult",
    "SearchEntry",
    # Sandbox models
    "SandboxInfo",
    "SandboxStatus",
    "SandboxState",
    "NetworkPolicy",
    "NetworkRule",
    "CredentialProxyConfig",
    "CredentialSubstitution",
    "InlineCredentialSource",
    "Credential",
    "CredentialMatch",
    "CustomHeaderEntry",
    "CredentialAuth",
    "CredentialBinding",
    "CredentialListResponse",
    "CredentialBindingListResponse",
    "CredentialMetadata",
    "CredentialBindingMetadata",
    "CredentialMutationSet",
    "CredentialBindingMutationSet",
    "CredentialVaultPatchRequest",
    "CredentialVaultState",
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
