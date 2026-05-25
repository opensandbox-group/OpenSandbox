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

"""
Pydantic schemas for OpenSandbox Lifecycle API.

This module defines data models based on the OpenAPI specification
for request/response validation and serialization.
"""

from datetime import datetime
from typing import Dict, List, Literal, Optional

from pydantic import BaseModel, Field, RootModel, model_validator


# ============================================================================
# Image Specification
# ============================================================================

class ImageAuth(BaseModel):
    """
    Registry authentication credentials for private container registries.
    """
    username: str = Field(..., description="Registry username or service account")
    password: str = Field(..., description="Registry password or authentication token")


class ImageSpec(BaseModel):
    """
    Container image specification for sandbox provisioning.

    Supports public registry images and private registry images with authentication.
    """
    uri: str = Field(
        ...,
        description="Container image URI in standard format (e.g., 'python:3.11', 'gcr.io/my-project/app:v1.0')",
    )
    auth: Optional[ImageAuth] = Field(
        None,
        description="Registry authentication credentials (required for private registries)",
    )


class PlatformSpec(BaseModel):
    """
    Runtime platform constraint for scheduling/provisioning.
    """

    os: str = Field(
        ...,
        description="Target operating system (for example 'linux').",
    )
    arch: str = Field(
        ...,
        description="Target CPU architecture (for example 'amd64' or 'arm64').",
    )


# ============================================================================
# Resource Limits
# ============================================================================

class ResourceLimits(RootModel[Dict[str, str]]):
    """
    Runtime resource constraints as key-value pairs.

    Similar to Kubernetes resource specifications, allows flexible definition
    of resource limits. Common resource types include cpu, memory, and gpu.
    """
    root: Dict[str, str] = Field(
        default_factory=dict,
        example={"cpu": "500m", "memory": "512Mi", "gpu": "1"},
    )


class NetworkRule(BaseModel):
    """
    Egress rule: allow/deny a specific domain or wildcard.
    """

    action: str = Field(..., description="Whether to allow or deny matching targets (allow | deny).")
    target: str = Field(
        ...,
        description="FQDN or wildcard domain (e.g., 'example.com', '*.example.com').",
        min_length=1,
    )

    class Config:
        populate_by_name = True


class NetworkPolicy(BaseModel):
    """
    Egress network policy matching the sidecar /policy payload.
    """

    default_action: Optional[str] = Field(
        default=None,
        alias="defaultAction",
        description="Default action when no egress rule matches (allow | deny). If omitted, sidecar defaults to deny.",
    )
    egress: list[NetworkRule] = Field(
        default_factory=list,
        description="Ordered egress rules. Empty/omitted yields allow-all at startup.",
    )

    class Config:
        populate_by_name = True


# ============================================================================
# Volume Definitions
# ============================================================================


class Host(BaseModel):
    """
    Host path bind mount backend.

    Maps a directory on the host filesystem into the container.
    Only available when the runtime supports host mounts.

    Security note: Host paths are restricted by server-side allowlist.
    Users must specify paths under permitted prefixes.
    """

    path: str = Field(
        ...,
        description="Absolute path on the host filesystem to mount.",
        pattern=r"^(/|[A-Za-z]:[\\/])",
    )


class PVC(BaseModel):
    """
    Platform-managed named volume backend.

    A runtime-neutral abstraction for referencing a platform-managed named volume.
    If ``createIfNotExists`` is true (the default) and the volume does not
    yet exist, it will be created automatically using the provisioning hints below.

    - Kubernetes: maps to a PersistentVolumeClaim in the same namespace.
    - Docker: maps to a Docker named volume (created via ``docker volume create``).
    """

    claim_name: str = Field(
        ...,
        alias="claimName",
        description=(
            "Name of the volume on the target platform. "
            "In Kubernetes this is the PVC name; in Docker this is the named volume name."
        ),
        pattern=r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$",
        max_length=253,
    )

    create_if_not_exists: bool = Field(
        True,
        alias="createIfNotExists",
        description=(
            "When true, the volume is automatically created if it does not exist. "
            "When false, referencing a non-existent volume fails with an error."
        ),
    )
    delete_on_sandbox_termination: bool = Field(
        False,
        alias="deleteOnSandboxTermination",
        description=(
            "When true, the volume is automatically removed when the sandbox is "
            "deleted. Only applies to volumes that were auto-created by the server "
            "(Docker only). Pre-existing volumes are never removed. Has no effect "
            "on Kubernetes PVCs, whose lifecycle is managed by the StorageClass "
            "reclaim policy."
        ),
    )

    # Provisioning hints — used only when auto-creating a new volume.
    # Ignored if the volume already exists on the platform.
    storage_class: Optional[str] = Field(
        None,
        alias="storageClass",
        description=(
            "Kubernetes StorageClass name for auto-created PVCs. "
            "None means use the cluster default. Ignored for Docker volumes."
        ),
    )
    storage: Optional[str] = Field(
        None,
        description=(
            "Storage capacity request for auto-created PVCs (e.g. '1Gi', '10Gi'). "
            "Defaults to server-side configured value when omitted. "
            "Ignored for Docker volumes."
        ),
        pattern=r"^\d+(\.\d+)?(Ki|Mi|Gi|Ti|Pi|Ei)?$",
    )
    access_modes: Optional[List[str]] = Field(
        None,
        alias="accessModes",
        description=(
            "Access modes for auto-created PVCs (e.g. ['ReadWriteOnce']). "
            "Defaults to ['ReadWriteOnce'] when omitted. Ignored for Docker volumes."
        ),
    )

    class Config:
        populate_by_name = True


class OSSFS(BaseModel):
    """
    Alibaba Cloud OSS mount backend via ossfs.

    The runtime mounts a host-side OSS path under ``storage.ossfs_mount_root``
    and then bind-mounts the resolved path into the sandbox container. Prefix
    selection is expressed via ``Volume.subPath``.
    In Docker runtime, OSSFS backend requires the server host to be Linux with FUSE support.
    """

    bucket: str = Field(
        ...,
        description="OSS bucket name.",
        min_length=3,
        max_length=63,
    )
    endpoint: str = Field(
        ...,
        description="OSS endpoint, e.g. 'oss-cn-hangzhou.aliyuncs.com'.",
        min_length=1,
    )
    version: Literal["1.0", "2.0"] = Field(
        "2.0",
        description="ossfs major version used by runtime mount integration.",
    )
    options: Optional[List[str]] = Field(
        None,
        description=(
            "Additional ossfs mount options. Runtime encodes options by version: "
            "1.0 => 'ossfs ... -o <option>', 2.0 => 'ossfs2 config line --<option>'. "
            "Provide raw option payloads without leading '-'."
        ),
    )
    access_key_id: Optional[str] = Field(
        None,
        alias="accessKeyId",
        description="OSS access key ID for inline credentials mode.",
        min_length=1,
    )
    access_key_secret: Optional[str] = Field(
        None,
        alias="accessKeySecret",
        description="OSS access key secret for inline credentials mode.",
        min_length=1,
    )
    class Config:
        populate_by_name = True

    @model_validator(mode="after")
    def validate_inline_credentials(self) -> "OSSFS":
        """Ensure inline credentials are provided for current OSSFS mode."""
        if not self.access_key_id or not self.access_key_secret:
            raise ValueError(
                "OSSFS inline credentials are required: accessKeyId and accessKeySecret."
            )
        return self


class Volume(BaseModel):
    """
    Storage mount definition for a sandbox.

    Each volume entry contains:
    - A unique name identifier
    - Exactly one backend struct (host, pvc, etc.) with backend-specific fields
    - Common mount settings (mountPath, readOnly, subPath)
    """

    name: str = Field(
        ...,
        description="Unique identifier for the volume within the sandbox.",
        pattern=r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$",
        max_length=63,
    )
    host: Optional[Host] = Field(
        None,
        description="Host path bind mount backend.",
    )
    pvc: Optional[PVC] = Field(
        None,
        description="Platform-managed named volume backend (PVC in Kubernetes, named volume in Docker).",
    )
    ossfs: Optional[OSSFS] = Field(
        None,
        description="OSSFS mount backend.",
    )
    mount_path: str = Field(
        ...,
        alias="mountPath",
        description="Absolute path inside the container where the volume is mounted.",
        pattern=r"^/.*",
    )
    read_only: bool = Field(
        False,
        alias="readOnly",
        description="If true, the volume is mounted as read-only. Defaults to false (read-write).",
    )
    sub_path: Optional[str] = Field(
        None,
        alias="subPath",
        description="Optional subdirectory under the backend path to mount.",
    )

    class Config:
        populate_by_name = True

    @model_validator(mode="after")
    def validate_exactly_one_backend(self) -> "Volume":
        """Ensure exactly one backend type is specified."""
        backends = [self.host, self.pvc, self.ossfs]
        specified = [b for b in backends if b is not None]
        if len(specified) == 0:
            raise ValueError("Exactly one backend (host, pvc, ossfs) must be specified, but none was provided.")
        if len(specified) > 1:
            raise ValueError("Exactly one backend (host, pvc, ossfs) must be specified, but multiple were provided.")
        return self


# ============================================================================
# Sandbox Status
# ============================================================================

class SandboxStatus(BaseModel):
    """
    Detailed status information with lifecycle state and transition details.
    """
    state: str = Field(
        ...,
        description="Current lifecycle state (Pending, Running, Pausing, Paused, Resuming, Stopping, Terminated, Failed)",
    )
    reason: Optional[str] = Field(
        None,
        description="Short machine-readable reason code for the current state",
    )
    message: Optional[str] = Field(
        None,
        description="Human-readable message describing the current state or reason for state transition",
    )
    last_transition_at: Optional[datetime] = Field(
        None,
        alias="lastTransitionAt",
        description="Timestamp of the last state transition",
    )

    class Config:
        populate_by_name = True


# ============================================================================
# Sandbox Models
# ============================================================================

class CreateSandboxRequest(BaseModel):
    """
    Request to create a new sandbox from either a container image or a snapshot.
    """
    image: Optional[ImageSpec] = Field(
        None,
        description="Container image specification for the sandbox",
    )
    snapshot_id: Optional[str] = Field(
        None,
        alias="snapshotId",
        description="Snapshot identifier to restore from",
    )
    platform: Optional[PlatformSpec] = Field(
        None,
        description=(
            "Optional platform constraint for sandbox scheduling/runtime selection. "
            "If omitted, runtime default behavior applies (runtime-specific and not a fixed "
            "architecture guarantee). If specified, runtime must satisfy this platform or fail "
            "explicitly."
        ),
    )
    timeout: Optional[int] = Field(
        None,
        ge=60,
        description=(
            "Sandbox timeout in seconds (minimum 60). "
            "The maximum is controlled by server.max_sandbox_timeout_seconds. "
            "When omitted or null, the sandbox will not auto-terminate and must be deleted explicitly. "
            "Note: manual cleanup support is runtime-dependent; Kubernetes providers may reject "
            "null timeout when the workload provider does not support non-expiring sandboxes."
        ),
    )
    resource_limits: Optional[ResourceLimits] = Field(
        None,
        alias="resourceLimits",
        description="Runtime resource constraints for the sandbox instance. Optional when poolRef is provided.",
    )
    env: Optional[Dict[str, Optional[str]]] = Field(
        None,
        description="Environment variables to inject into the sandbox runtime",
    )
    metadata: Optional[Dict[str, str]] = Field(
        None,
        description="Custom key-value metadata for management, filtering, and tagging",
    )
    entrypoint: Optional[List[str]] = Field(
        None,
        min_length=1,
        description=(
            "The command to execute as the sandbox's entry process. "
            "Required when image is provided. Optional when snapshotId is provided; "
            'the server defaults to ["tail", "-f", "/dev/null"] when omitted.'
        ),
        example=["python", "/app/main.py"],
    )
    network_policy: Optional[NetworkPolicy] = Field(
        None,
        alias="networkPolicy",
        description=(
            "Optional outbound network policy. Shape matches the egress sidecar /policy endpoint. "
            "Empty/omitted means allow-all until updated."
        ),
    )
    secure_access: bool = Field(
        False,
        alias="secureAccess",
        description=(
            "Opts the sandbox into secured access for endpoint access. "
            "Currently supported only for Kubernetes sandboxes exposed through ingress gateway mode. "
            "When enabled, the server provisions access credentials and returns required endpoint headers."
        ),
    )
    volumes: Optional[List[Volume]] = Field(
        None,
        description=(
            "Storage mounts for the sandbox. Each volume entry specifies a named backend-specific "
            "storage source and common mount settings. Exactly one backend type must be specified per volume entry."
        ),
    )
    extensions: Optional[Dict[str, str]] = Field(
        None,
        description="Opaque container for provider-specific or transient parameters not covered by the core API",
    )

    @model_validator(mode="after")
    def validate_source_and_entrypoint(self) -> "CreateSandboxRequest":
        # When poolRef is set, image/snapshotId/entrypoint/resourceLimits are
        # all defined in the Pool CRD and not required from the caller.
        has_pool_ref = bool((self.extensions or {}).get("poolRef", "").strip())
        if has_pool_ref:
            # Reject conflicting fields that would be ignored in pool mode
            if bool((self.snapshot_id or "").strip()):
                raise ValueError("snapshotId cannot be used together with poolRef.")
            # Normalize blank snapshotId so downstream code won't see
            # a truthy whitespace string (e.g. "   ") as a real value.
            if self.snapshot_id is not None and not self.snapshot_id.strip():
                self.snapshot_id = None
            return self

        has_image = self.image is not None and bool(self.image.uri.strip())
        has_snapshot = bool((self.snapshot_id or "").strip())

        if has_image == has_snapshot:
            raise ValueError("Exactly one of image or snapshotId must be provided.")

        if has_image and not self.entrypoint:
            raise ValueError("Entrypoint is required when image is provided.")

        if self.image is not None and not has_image:
            self.image = None

        if self.snapshot_id is not None and not has_snapshot:
            self.snapshot_id = None

        if self.resource_limits is None:
            raise ValueError("resourceLimits is required when poolRef is not provided.")

        return self

    class Config:
        populate_by_name = True


class CreateSandboxResponse(BaseModel):
    """
    Response from creating a new sandbox.

    Contains essential information without image and updatedAt.
    """
    id: str = Field(..., description="Unique sandbox identifier")
    status: SandboxStatus = Field(..., description="Current lifecycle status and detailed state information")
    metadata: Optional[Dict[str, str]] = Field(None, description="Custom metadata from creation request")
    platform: Optional[PlatformSpec] = Field(
        None,
        description=(
            "Platform constraint echoed from request or workload template. "
            "Null when no scheduling constraint is provided."
        ),
    )
    expires_at: Optional[datetime] = Field(
        None,
        alias="expiresAt",
        description="Timestamp when sandbox will auto-terminate. Null when manual cleanup is enabled.",
    )
    created_at: datetime = Field(..., alias="createdAt", description="Sandbox creation timestamp")
    entrypoint: Optional[List[str]] = Field(None, description="Entry process specification from creation request")

    class Config:
        populate_by_name = True


class Sandbox(BaseModel):
    """
    Runtime execution environment provisioned from a container image.

    This is the complete representation of the sandbox resource.
    """
    id: str = Field(..., description="Unique sandbox identifier")
    image: Optional[ImageSpec] = Field(None, description="Container image specification used to provision this sandbox")
    snapshot_id: Optional[str] = Field(
        None,
        alias="snapshotId",
        description="Snapshot identifier used to restore this sandbox",
    )
    platform: Optional[PlatformSpec] = Field(
        None,
        description=(
            "Platform constraint echoed from request or workload template. "
            "Null when no scheduling constraint is provided."
        ),
    )
    status: SandboxStatus = Field(..., description="Current lifecycle status and detailed state information")
    metadata: Optional[Dict[str, str]] = Field(None, description="Custom metadata from creation request")
    entrypoint: Optional[List[str]] = Field(None, description="The command to execute as the sandbox's entry process")
    expires_at: Optional[datetime] = Field(
        None,
        alias="expiresAt",
        description="Timestamp when sandbox will auto-terminate. Null when manual cleanup is enabled.",
    )
    created_at: datetime = Field(..., alias="createdAt", description="Sandbox creation timestamp")

    class Config:
        populate_by_name = True


PatchSandboxMetadataRequest = Dict[str, Optional[str]]
"""Metadata merge-patch body: non-null values add/replace, null values delete, absent keys unchanged."""


# Snapshot Models
# ============================================================================

class SnapshotStatus(BaseModel):
    """
    Detailed snapshot status information with lifecycle state and transition details.
    """
    state: str = Field(
        ...,
        description="Current snapshot lifecycle state (Creating, Deleting, Ready, Failed)",
    )
    reason: Optional[str] = Field(
        None,
        description="Short machine-readable reason code for the current state",
    )
    message: Optional[str] = Field(
        None,
        description="Human-readable message describing the current state or failure reason",
    )
    last_transition_at: Optional[datetime] = Field(
        None,
        alias="lastTransitionAt",
        description="Timestamp of the last state transition",
    )

    class Config:
        populate_by_name = True


class CreateSnapshotRequest(BaseModel):
    """
    Request to create a snapshot from a sandbox.
    """
    name: Optional[str] = Field(
        None,
        min_length=1,
        description="Optional human-readable snapshot name",
    )


class Snapshot(BaseModel):
    """
    Persistent point-in-time capture of a sandbox.
    """
    id: str = Field(..., description="Unique snapshot identifier")
    sandbox_id: str = Field(
        ...,
        alias="sandboxId",
        description="Source sandbox identifier used to create this snapshot",
    )
    name: Optional[str] = Field(
        None,
        description="Optional human-readable snapshot name",
    )
    status: SnapshotStatus = Field(
        ...,
        description="Current snapshot lifecycle status and detailed state information",
    )
    created_at: datetime = Field(
        ...,
        alias="createdAt",
        description="Snapshot creation timestamp",
    )

    class Config:
        populate_by_name = True


class SnapshotFilter(BaseModel):
    """
    Filtering criteria for listing snapshots.
    """
    sandbox_id: Optional[str] = Field(
        None,
        alias="sandboxId",
        description="Filter snapshots by source sandbox identifier",
    )
    state: Optional[List[str]] = Field(
        None,
        min_length=1,
        description="Filter by snapshot lifecycle state (status.state) - supports OR logic",
    )

    class Config:
        populate_by_name = True


class ListSnapshotsRequest(BaseModel):
    """
    Request body for snapshot listing queries.
    """
    filter: SnapshotFilter = Field(
        default_factory=SnapshotFilter,
        description="Filtering criteria (all conditions combined with AND logic)",
    )
    pagination: Optional["PaginationRequest"] = Field(None, description="Pagination parameters")


class ListSnapshotsResponse(BaseModel):
    """
    Paginated collection of snapshots.
    """
    items: List[Snapshot] = Field(..., description="List of snapshots")
    pagination: "PaginationInfo" = Field(..., description="Pagination metadata")


# ============================================================================
# List Sandboxes
# ============================================================================

class SandboxFilter(BaseModel):
    """
    Filtering criteria for listing sandboxes.
    """
    state: Optional[List[str]] = Field(
        None,
        min_length=1,
        description="Filter by lifecycle state (status.state) - supports OR logic",
    )
    metadata: Optional[Dict[str, str]] = Field(
        None,
        description="Filter by metadata key-value pairs (AND logic)",
    )


class PaginationRequest(BaseModel):
    """
    Pagination parameters for list requests.
    """
    page: int = Field(1, ge=1, description="Page number")
    page_size: int = Field(
        20,
        ge=1,
        le=200,
        alias="pageSize",
        description="Number of items per page",
    )

    class Config:
        populate_by_name = True


class ListSandboxesRequest(BaseModel):
    """
    Request body for complex listing queries.
    """
    filter: SandboxFilter = Field(
        default_factory=SandboxFilter,
        description="Filtering criteria (all conditions combined with AND logic)",
    )
    pagination: Optional[PaginationRequest] = Field(None, description="Pagination parameters")


class PaginationInfo(BaseModel):
    """
    Pagination metadata for list responses.
    """
    page: int = Field(..., ge=1, description="Current page number")
    page_size: int = Field(..., ge=1, alias="pageSize", description="Number of items per page")
    total_items: int = Field(..., ge=0, alias="totalItems", description="Total number of items matching the filter")
    total_pages: int = Field(..., ge=0, alias="totalPages", description="Total number of pages")
    has_next_page: bool = Field(..., alias="hasNextPage", description="Whether there are more pages after the current one")

    class Config:
        populate_by_name = True


class ListSandboxesResponse(BaseModel):
    """
    Paginated collection of sandboxes.
    """
    items: List[Sandbox] = Field(..., description="List of sandboxes")
    pagination: PaginationInfo = Field(..., description="Pagination metadata")


# ============================================================================
# Renew Expiration
# ============================================================================

class RenewSandboxExpirationRequest(BaseModel):
    """
    Request to renew sandbox expiration time.
    """
    expires_at: datetime = Field(
        ...,
        alias="expiresAt",
        description="New absolute expiration time in UTC (RFC 3339 format). Must be in the future.",
    )

    class Config:
        populate_by_name = True


class RenewSandboxExpirationResponse(BaseModel):
    """
    Response for renewing sandbox expiration.
    """
    expires_at: datetime = Field(
        ...,
        alias="expiresAt",
        description="The new absolute expiration time in UTC (RFC 3339 format)",
    )

    class Config:
        populate_by_name = True


# ============================================================================
# Endpoint
# ============================================================================

class Endpoint(BaseModel):
    """
    Endpoint for accessing a service running in the sandbox.
    """
    endpoint: str = Field(
        ...,
        description="Public endpoint string (host[:port]/path) exposed for the sandbox service",
    )
    headers: Optional[dict[str, str]] = Field(
        default=None,
        description="Optional headers required when accessing the endpoint (e.g., for header-based routing).",
    )
    class Config:
        populate_by_name = True


# ============================================================================
# Error Response
# ============================================================================

class ErrorResponse(BaseModel):
    """
    Standard error response for all non-2xx HTTP responses.

    HTTP status code indicates the error category; code and message provide details.
    """
    code: str = Field(
        ...,
        description="Machine-readable error code (e.g., INVALID_REQUEST, NOT_FOUND, INTERNAL_ERROR)",
    )
    message: str = Field(
        ...,
        description="Human-readable error message describing what went wrong and how to fix it",
    )


# ============================================================================
# Pool Models
# ============================================================================

class PoolCapacitySpec(BaseModel):
    """
    Capacity configuration that controls the size of the resource pool.
    """
    buffer_max: int = Field(
        ...,
        alias="bufferMax",
        ge=0,
        description="Maximum number of nodes kept in the warm buffer.",
    )
    buffer_min: int = Field(
        ...,
        alias="bufferMin",
        ge=0,
        description="Minimum number of nodes that must remain in the buffer.",
    )
    pool_max: int = Field(
        ...,
        alias="poolMax",
        ge=0,
        description="Maximum total number of nodes allowed in the entire pool.",
    )
    pool_min: int = Field(
        ...,
        alias="poolMin",
        ge=0,
        description="Minimum total size of the pool.",
    )

    class Config:
        populate_by_name = True


class CreatePoolRequest(BaseModel):
    """
    Request to create a new pre-warmed resource pool.

    A Pool manages a set of pre-warmed pods that can be rapidly allocated
    to sandboxes, reducing cold-start latency.
    """
    name: str = Field(
        ...,
        description="Unique name for the pool (must be a valid Kubernetes resource name).",
        pattern=r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$",
        max_length=253,
    )
    template: Dict = Field(
        ...,
        description=(
            "Kubernetes PodTemplateSpec defining the pod configuration for pre-warmed nodes. "
            "Follows the same schema as spec.template in a Kubernetes Deployment."
        ),
    )
    capacity_spec: PoolCapacitySpec = Field(
        ...,
        alias="capacitySpec",
        description="Capacity configuration controlling pool size and buffer behavior.",
    )

    class Config:
        populate_by_name = True


class UpdatePoolRequest(BaseModel):
    """
    Request to update an existing pool's capacity configuration.

    Only capacity settings can be updated after pool creation.
    Updating the pod template requires recreating the pool.
    """
    capacity_spec: PoolCapacitySpec = Field(
        ...,
        alias="capacitySpec",
        description="New capacity configuration for the pool.",
    )

    class Config:
        populate_by_name = True


class PoolStatus(BaseModel):
    """
    Observed runtime state of a pool.
    """
    total: int = Field(..., description="Total number of nodes in the pool.")
    allocated: int = Field(..., description="Number of nodes currently allocated to sandboxes.")
    available: int = Field(..., description="Number of nodes currently available in the pool.")
    revision: str = Field(..., description="Latest revision identifier of the pool.")


class PoolResponse(BaseModel):
    """
    Full representation of a Pool resource.
    """
    name: str = Field(..., description="Unique pool name.")
    capacity_spec: PoolCapacitySpec = Field(
        ...,
        alias="capacitySpec",
        description="Capacity configuration of the pool.",
    )
    status: Optional[PoolStatus] = Field(
        None,
        description="Observed runtime state of the pool. May be absent if not yet reconciled.",
    )
    created_at: Optional[datetime] = Field(
        None,
        alias="createdAt",
        description="Pool creation timestamp.",
    )

    class Config:
        populate_by_name = True


class ListPoolsResponse(BaseModel):
    """
    Collection of pools.
    """
    items: List[PoolResponse] = Field(..., description="List of pools.")
