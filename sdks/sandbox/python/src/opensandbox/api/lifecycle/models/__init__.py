#
# Copyright 2026 The OpenSandbox Authors
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

from .activity_probe import ActivityProbe
from .allocation_summary import AllocationSummary
from .allocation_summary_mode import AllocationSummaryMode
from .allocation_summary_state import AllocationSummaryState
from .api_key_credential_auth import ApiKeyCredentialAuth
from .api_key_credential_auth_type import ApiKeyCredentialAuthType
from .basic_credential_auth import BasicCredentialAuth
from .basic_credential_auth_type import BasicCredentialAuthType
from .bearer_credential_auth import BearerCredentialAuth
from .bearer_credential_auth_type import BearerCredentialAuthType
from .create_fsb_template_request import CreateFsbTemplateRequest
from .create_fsb_template_request_env import CreateFsbTemplateRequestEnv
from .create_fsb_template_request_format import CreateFsbTemplateRequestFormat
from .create_fsb_template_request_metadata import CreateFsbTemplateRequestMetadata
from .create_sandbox_request import CreateSandboxRequest
from .create_sandbox_request_env import CreateSandboxRequestEnv
from .create_sandbox_request_extensions import CreateSandboxRequestExtensions
from .create_sandbox_request_metadata import CreateSandboxRequestMetadata
from .create_sandbox_response import CreateSandboxResponse
from .create_sandbox_response_extensions import CreateSandboxResponseExtensions
from .create_sandbox_response_metadata import CreateSandboxResponseMetadata
from .create_snapshot_request import CreateSnapshotRequest
from .credential import Credential
from .credential_auth_metadata import CredentialAuthMetadata
from .credential_binding import CredentialBinding
from .credential_binding_list_response import CredentialBindingListResponse
from .credential_binding_metadata import CredentialBindingMetadata
from .credential_binding_mutation_set import CredentialBindingMutationSet
from .credential_list_response import CredentialListResponse
from .credential_match import CredentialMatch
from .credential_match_schemes_item import CredentialMatchSchemesItem
from .credential_metadata import CredentialMetadata
from .credential_mutation_set import CredentialMutationSet
from .credential_proxy_config import CredentialProxyConfig
from .credential_substitution import CredentialSubstitution
from .credential_substitution_in_item import CredentialSubstitutionInItem
from .credential_vault_create_request import CredentialVaultCreateRequest
from .credential_vault_mutation_request import CredentialVaultMutationRequest
from .credential_vault_state import CredentialVaultState
from .custom_header_entry import CustomHeaderEntry
from .custom_headers_credential_auth import CustomHeadersCredentialAuth
from .custom_headers_credential_auth_type import CustomHeadersCredentialAuthType
from .endpoint import Endpoint
from .endpoint_headers import EndpointHeaders
from .error_response import ErrorResponse
from .fsb_template import FsbTemplate
from .fsb_template_env import FsbTemplateEnv
from .fsb_template_format import FsbTemplateFormat
from .fsb_template_metadata import FsbTemplateMetadata
from .fsb_template_readiness import FsbTemplateReadiness
from .fsb_template_status import FsbTemplateStatus
from .fsb_template_status_phase import FsbTemplateStatusPhase
from .host import Host
from .idle_policy import IdlePolicy
from .idle_policy_on_idle import IdlePolicyOnIdle
from .image_spec import ImageSpec
from .image_spec_auth import ImageSpecAuth
from .inline_credential_source import InlineCredentialSource
from .inline_credential_source_type import InlineCredentialSourceType
from .lifecycle_hook import LifecycleHook
from .list_fsb_templates_response import ListFsbTemplatesResponse
from .list_sandboxes_response import ListSandboxesResponse
from .list_snapshots_response import ListSnapshotsResponse
from .metrics_event import MetricsEvent
from .metrics_event_event_type import MetricsEventEventType
from .network_policy import NetworkPolicy
from .network_policy_default_action import NetworkPolicyDefaultAction
from .network_rule import NetworkRule
from .network_rule_action import NetworkRuleAction
from .ossfs import OSSFS
from .ossfs_version import OSSFSVersion
from .pagination_info import PaginationInfo
from .passthrough_credential_auth import PassthroughCredentialAuth
from .passthrough_credential_auth_type import PassthroughCredentialAuthType
from .patch_sandbox_metadata_request import PatchSandboxMetadataRequest
from .periodic_lifecycle_hook import PeriodicLifecycleHook
from .platform_spec import PlatformSpec
from .platform_spec_arch import PlatformSpecArch
from .platform_spec_os import PlatformSpecOs
from .policy_status_response import PolicyStatusResponse
from .pvc import PVC
from .renew_sandbox_expiration_request import RenewSandboxExpirationRequest
from .renew_sandbox_expiration_response import RenewSandboxExpirationResponse
from .resource_limits import ResourceLimits
from .sandbox import Sandbox
from .sandbox_extensions import SandboxExtensions
from .sandbox_lifecycle import SandboxLifecycle
from .sandbox_metadata import SandboxMetadata
from .sandbox_status import SandboxStatus
from .snapshot import Snapshot
from .snapshot_status import SnapshotStatus
from .volume import Volume

__all__ = (
    "ActivityProbe",
    "AllocationSummary",
    "AllocationSummaryMode",
    "AllocationSummaryState",
    "ApiKeyCredentialAuth",
    "ApiKeyCredentialAuthType",
    "BasicCredentialAuth",
    "BasicCredentialAuthType",
    "BearerCredentialAuth",
    "BearerCredentialAuthType",
    "CreateFsbTemplateRequest",
    "CreateFsbTemplateRequestEnv",
    "CreateFsbTemplateRequestFormat",
    "CreateFsbTemplateRequestMetadata",
    "CreateSandboxRequest",
    "CreateSandboxRequestEnv",
    "CreateSandboxRequestExtensions",
    "CreateSandboxRequestMetadata",
    "CreateSandboxResponse",
    "CreateSandboxResponseExtensions",
    "CreateSandboxResponseMetadata",
    "CreateSnapshotRequest",
    "Credential",
    "CredentialAuthMetadata",
    "CredentialBinding",
    "CredentialBindingListResponse",
    "CredentialBindingMetadata",
    "CredentialBindingMutationSet",
    "CredentialListResponse",
    "CredentialMatch",
    "CredentialMatchSchemesItem",
    "CredentialMetadata",
    "CredentialMutationSet",
    "CredentialProxyConfig",
    "CredentialSubstitution",
    "CredentialSubstitutionInItem",
    "CredentialVaultCreateRequest",
    "CredentialVaultMutationRequest",
    "CredentialVaultState",
    "CustomHeaderEntry",
    "CustomHeadersCredentialAuth",
    "CustomHeadersCredentialAuthType",
    "Endpoint",
    "EndpointHeaders",
    "ErrorResponse",
    "FsbTemplate",
    "FsbTemplateEnv",
    "FsbTemplateFormat",
    "FsbTemplateMetadata",
    "FsbTemplateReadiness",
    "FsbTemplateStatus",
    "FsbTemplateStatusPhase",
    "Host",
    "IdlePolicy",
    "IdlePolicyOnIdle",
    "ImageSpec",
    "ImageSpecAuth",
    "InlineCredentialSource",
    "InlineCredentialSourceType",
    "LifecycleHook",
    "ListFsbTemplatesResponse",
    "ListSandboxesResponse",
    "ListSnapshotsResponse",
    "MetricsEvent",
    "MetricsEventEventType",
    "NetworkPolicy",
    "NetworkPolicyDefaultAction",
    "NetworkRule",
    "NetworkRuleAction",
    "OSSFS",
    "OSSFSVersion",
    "PaginationInfo",
    "PassthroughCredentialAuth",
    "PassthroughCredentialAuthType",
    "PatchSandboxMetadataRequest",
    "PeriodicLifecycleHook",
    "PlatformSpec",
    "PlatformSpecArch",
    "PlatformSpecOs",
    "PolicyStatusResponse",
    "PVC",
    "RenewSandboxExpirationRequest",
    "RenewSandboxExpirationResponse",
    "ResourceLimits",
    "Sandbox",
    "SandboxExtensions",
    "SandboxLifecycle",
    "SandboxMetadata",
    "SandboxStatus",
    "Snapshot",
    "SnapshotStatus",
    "Volume",
)
