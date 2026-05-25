#
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
#

"""Contains all the data models used in inputs/outputs"""

from .create_sandbox_request import CreateSandboxRequest
from .create_sandbox_request_env import CreateSandboxRequestEnv
from .create_sandbox_request_extensions import CreateSandboxRequestExtensions
from .create_sandbox_request_metadata import CreateSandboxRequestMetadata
from .create_sandbox_response import CreateSandboxResponse
from .create_sandbox_response_metadata import CreateSandboxResponseMetadata
from .create_snapshot_request import CreateSnapshotRequest
from .endpoint import Endpoint
from .endpoint_headers import EndpointHeaders
from .error_response import ErrorResponse
from .host import Host
from .image_spec import ImageSpec
from .image_spec_auth import ImageSpecAuth
from .list_sandboxes_response import ListSandboxesResponse
from .list_snapshots_response import ListSnapshotsResponse
from .network_policy import NetworkPolicy
from .network_policy_default_action import NetworkPolicyDefaultAction
from .network_rule import NetworkRule
from .network_rule_action import NetworkRuleAction
from .ossfs import OSSFS
from .ossfs_version import OSSFSVersion
from .pagination_info import PaginationInfo
from .patch_sandbox_metadata_request import PatchSandboxMetadataRequest
from .platform_spec import PlatformSpec
from .platform_spec_arch import PlatformSpecArch
from .platform_spec_os import PlatformSpecOs
from .pvc import PVC
from .renew_sandbox_expiration_request import RenewSandboxExpirationRequest
from .renew_sandbox_expiration_response import RenewSandboxExpirationResponse
from .resource_limits import ResourceLimits
from .sandbox import Sandbox
from .sandbox_metadata import SandboxMetadata
from .sandbox_status import SandboxStatus
from .snapshot import Snapshot
from .snapshot_status import SnapshotStatus
from .volume import Volume

__all__ = (
    "CreateSandboxRequest",
    "CreateSandboxRequestEnv",
    "CreateSandboxRequestExtensions",
    "CreateSandboxRequestMetadata",
    "CreateSandboxResponse",
    "CreateSandboxResponseMetadata",
    "CreateSnapshotRequest",
    "Endpoint",
    "EndpointHeaders",
    "ErrorResponse",
    "Host",
    "ImageSpec",
    "ImageSpecAuth",
    "ListSandboxesResponse",
    "ListSnapshotsResponse",
    "NetworkPolicy",
    "NetworkPolicyDefaultAction",
    "NetworkRule",
    "NetworkRuleAction",
    "OSSFS",
    "OSSFSVersion",
    "PaginationInfo",
    "PatchSandboxMetadataRequest",
    "PlatformSpec",
    "PlatformSpecArch",
    "PlatformSpecOs",
    "PVC",
    "RenewSandboxExpirationRequest",
    "RenewSandboxExpirationResponse",
    "ResourceLimits",
    "Sandbox",
    "SandboxMetadata",
    "SandboxStatus",
    "Snapshot",
    "SnapshotStatus",
    "Volume",
)
