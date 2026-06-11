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
Sandbox-related data models.

Models for sandbox creation, configuration, status, and lifecycle management.
"""

import re
from datetime import datetime
from typing import Literal

from pydantic import BaseModel, ConfigDict, Field, field_validator, model_validator


class SandboxImageAuth(BaseModel):
    """
    Authentication credentials for container registries.
    """

    username: str = Field(description="Registry username")
    password: str = Field(description="Registry password or access token")

    @field_validator("username")
    @classmethod
    def username_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Username cannot be blank")
        return v

    @field_validator("password")
    @classmethod
    def password_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Password cannot be blank")
        return v


class SandboxImageSpec(BaseModel):
    """
    Specification for a sandbox container image.

    Usage:
        # Simple creation with just image
        spec = SandboxImageSpec("python:3.11")

        # With private registry auth
        spec = SandboxImageSpec(
            "my-registry.com/image:tag",
            auth=SandboxImageAuth(username="user", password="pass")
        )
    """

    image: str = Field(
        description="Image reference (e.g., 'ubuntu:22.04', 'python:3.11')"
    )
    auth: SandboxImageAuth | None = Field(
        default=None, description="Authentication for private registries"
    )

    def __init__(
        self, image: str | None = None, *, auth: SandboxImageAuth | None = None, **data: object
    ) -> None:
        """
        Initialize SandboxImageSpec.

        Args:
            image: Container image reference (positional or keyword)
            auth: Optional authentication for private registries
        """
        if image is not None:
            data["image"] = image
        if auth is not None:
            data["auth"] = auth
        super().__init__(**data)

    @field_validator("image")
    @classmethod
    def image_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Image cannot be blank")
        return v


class PlatformSpec(BaseModel):
    """Runtime platform constraint for sandbox provisioning."""

    os: Literal["linux", "windows"] = Field(
        description="Target operating system for sandbox provisioning."
    )
    arch: Literal["amd64", "arm64"] = Field(
        description="Target CPU architecture for sandbox provisioning."
    )


class NetworkRule(BaseModel):
    """
    Egress rule for matching network targets.
    """

    action: Literal["allow", "deny"] = Field(
        description='Whether to allow or deny matching targets. One of "allow" or "deny".'
    )
    target: str = Field(
        description='FQDN or wildcard domain (e.g., "example.com", "*.example.com").'
    )

    @field_validator("target")
    @classmethod
    def target_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Network rule target cannot be blank")
        return v


class NetworkPolicy(BaseModel):
    """
    Egress network policy matching the sidecar `/policy` request body.
    """

    default_action: Literal["allow", "deny"] | None = Field(
        default="deny",
        description='Default action when no rule matches. Defaults to "deny".',
        alias="defaultAction",
    )
    egress: list[NetworkRule] | None = Field(
        default=None,
        description="List of egress rules evaluated in order.",
    )

    model_config = ConfigDict(populate_by_name=True)


# ============================================================================
# Volume Models
# ============================================================================

# Matches Unix absolute paths (/…) and Windows drive-letter paths (C:\ or C:/).
# Aligned with server-side pattern in server/opensandbox_server/api/schema.py.
_HOST_PATH_RE = re.compile(r"^(/|[A-Za-z]:[\\/])")

class Host(BaseModel):
    """
    Host path bind mount backend.

    Maps a directory on the host filesystem into the container.
    Only available when the runtime supports host mounts.
    """

    path: str = Field(
        description="Absolute path on the host filesystem to mount."
    )

    @field_validator("path")
    @classmethod
    def path_must_be_absolute(cls, v: str) -> str:
        if not _HOST_PATH_RE.match(v):
            raise ValueError(
                "Host path must be an absolute path starting with '/' "
                "or a Windows drive letter (e.g. 'C:\\' or 'D:/')"
            )
        return v


class PVC(BaseModel):
    """
    Platform-managed named volume backend.

    Runtime-neutral abstraction for referencing a pre-existing named volume:
    - Kubernetes: maps to a PersistentVolumeClaim in the same namespace.
    - Docker: maps to a Docker named volume.
    """

    claim_name: str = Field(
        description=(
            "Name of the platform volume. In Kubernetes this is the PVC name; "
            "in Docker this is the named volume name."
        ),
        alias="claimName",
    )
    create_if_not_exists: bool = Field(
        default=True,
        alias="createIfNotExists",
        description="When true, auto-create the volume if it does not exist.",
    )
    delete_on_sandbox_termination: bool = Field(
        default=False,
        alias="deleteOnSandboxTermination",
        description=(
            "When true, the auto-created volume (Docker named volume or "
            "Kubernetes PVC) is removed on sandbox deletion. Pre-existing "
            "volumes are never removed."
        ),
    )
    storage_class: str | None = Field(
        default=None,
        alias="storageClass",
        description=(
            "Kubernetes StorageClass for auto-created PVCs. "
            "Null means cluster default. Ignored for Docker."
        ),
    )
    storage: str | None = Field(
        default=None,
        description=(
            "Storage capacity request for auto-created PVCs (e.g. '1Gi'). "
            "Ignored for Docker."
        ),
    )
    access_modes: list[str] | None = Field(
        default=None,
        alias="accessModes",
        description=(
            "Access modes for auto-created PVCs (e.g. ['ReadWriteOnce']). "
            "Ignored for Docker."
        ),
    )

    model_config = ConfigDict(populate_by_name=True)

    @field_validator("claim_name")
    @classmethod
    def claim_name_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("PVC claim name cannot be blank")
        return v


class OSSFS(BaseModel):
    """Alibaba Cloud OSS mount backend via ossfs."""

    bucket: str = Field(description="OSS bucket name.")
    endpoint: str = Field(description="OSS endpoint (e.g., oss-cn-hangzhou.aliyuncs.com).")
    version: Literal["1.0", "2.0"] = Field(
        default="2.0",
        description="ossfs major version used by runtime mount integration.",
    )
    options: list[str] | None = Field(
        default=None,
        description="Additional ossfs mount options.",
    )
    access_key_id: str | None = Field(
        default=None,
        alias="accessKeyId",
        description="OSS access key ID for inline credentials mode.",
    )
    access_key_secret: str | None = Field(
        default=None,
        alias="accessKeySecret",
        description="OSS access key secret for inline credentials mode.",
    )
    model_config = ConfigDict(populate_by_name=True)

    @model_validator(mode="after")
    def validate_inline_credentials(self) -> "OSSFS":
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
    - Exactly one backend (host, pvc, ossfs) with backend-specific fields
    - Common mount settings (mount_path, read_only, sub_path)

    Usage:
        # Host path mount (read-write by default)
        volume = Volume(
            name="workdir",
            host=Host(path="/data/opensandbox"),
            mount_path="/mnt/work",
        )

        # PVC mount (read-only)
        volume = Volume(
            name="models",
            pvc=PVC(claim_name="shared-models-pvc"),
            mount_path="/mnt/models",
            read_only=True,
        )
    """

    name: str = Field(
        description="Unique identifier for the volume within the sandbox."
    )
    host: Host | None = Field(
        default=None,
        description="Host path bind mount backend.",
    )
    pvc: PVC | None = Field(
        default=None,
        description="Kubernetes PersistentVolumeClaim mount backend.",
    )
    ossfs: OSSFS | None = Field(
        default=None,
        description="OSSFS mount backend.",
    )
    mount_path: str = Field(
        description="Absolute path inside the container where the volume is mounted.",
        alias="mountPath",
    )
    read_only: bool = Field(
        default=False,
        description="If true, the volume is mounted as read-only. Defaults to false (read-write).",
        alias="readOnly",
    )
    sub_path: str | None = Field(
        default=None,
        description="Optional subdirectory under the backend path to mount.",
        alias="subPath",
    )

    model_config = ConfigDict(populate_by_name=True)

    @field_validator("name")
    @classmethod
    def name_must_not_be_empty(cls, v: str) -> str:
        if not v.strip():
            raise ValueError("Volume name cannot be blank")
        return v

    @field_validator("mount_path")
    @classmethod
    def mount_path_must_be_absolute(cls, v: str) -> str:
        if not v.startswith("/"):
            raise ValueError("Mount path must be an absolute path starting with '/'")
        return v

    @model_validator(mode="after")
    def validate_exactly_one_backend(self) -> "Volume":
        """Ensure exactly one backend (host, pvc, or ossfs) is specified."""
        backends = [self.host, self.pvc, self.ossfs]
        specified = [b for b in backends if b is not None]
        if len(specified) == 0:
            raise ValueError(
                "Exactly one backend (host, pvc, ossfs) must be specified, but none was provided."
            )
        if len(specified) > 1:
            raise ValueError(
                "Exactly one backend (host, pvc, ossfs) must be specified, but multiple were provided."
            )
        return self


class SandboxStatus(BaseModel):
    """
    Status information for a sandbox.
    """

    state: str = Field(
        description="Current state (e.g., RUNNING, PENDING, PAUSED, TERMINATED)"
    )
    reason: str | None = Field(
        default=None, description="Short reason code for current state"
    )
    message: str | None = Field(
        default=None, description="Human-readable status message"
    )
    last_transition_at: datetime | None = Field(
        default=None,
        description="Timestamp of last state transition",
        alias="last_transition_at",
    )

    model_config = ConfigDict(populate_by_name=True)


class SnapshotStatus(BaseModel):
    """
    Status information for a snapshot.
    """

    state: str = Field(description="Current snapshot lifecycle state")
    reason: str | None = Field(
        default=None, description="Short reason code for current state"
    )
    message: str | None = Field(
        default=None, description="Human-readable status message"
    )
    last_transition_at: datetime | None = Field(
        default=None,
        description="Timestamp of last state transition",
        alias="last_transition_at",
    )

    model_config = ConfigDict(populate_by_name=True)


class SnapshotInfo(BaseModel):
    """
    Detailed information about a snapshot instance.
    """

    id: str = Field(description="Unique identifier of the snapshot")
    sandbox_id: str = Field(
        description="Source sandbox identifier used to create this snapshot",
        alias="sandbox_id",
    )
    name: str | None = Field(default=None, description="Optional snapshot name")
    status: SnapshotStatus = Field(description="Current status of the snapshot")
    created_at: datetime = Field(description="Creation timestamp", alias="created_at")

    model_config = ConfigDict(populate_by_name=True)


class SandboxInfo(BaseModel):
    """
    Detailed information about a sandbox instance.
    """

    id: str = Field(description="Unique identifier of the sandbox")
    status: SandboxStatus = Field(description="Current status of the sandbox")
    entrypoint: list[str] = Field(
        description="Command line arguments used to start the sandbox"
    )
    expires_at: datetime | None = Field(
        default=None,
        description="Scheduled termination timestamp. Null means manual cleanup mode.",
        alias="expires_at",
    )
    created_at: datetime = Field(description="Creation timestamp", alias="created_at")
    image: SandboxImageSpec | None = Field(
        default=None, description="Image specification used to create sandbox"
    )
    snapshot_id: str | None = Field(
        default=None,
        description="Snapshot identifier used to restore sandbox",
        alias="snapshot_id",
    )
    platform: PlatformSpec | None = Field(
        default=None, description="Effective platform used for sandbox provisioning."
    )
    metadata: dict[str, str] | None = Field(default=None, description="Custom metadata")

    model_config = ConfigDict(populate_by_name=True)


class SandboxCreateResponse(BaseModel):
    """
    Response returned when a sandbox is created.
    """

    id: str = Field(description="Unique identifier of the newly created sandbox")
    platform: PlatformSpec | None = Field(
        default=None, description="Effective platform used for sandbox provisioning."
    )


class CreateSnapshotRequest(BaseModel):
    """
    Request returned when creating a snapshot.
    """

    name: str | None = Field(default=None, description="Optional snapshot name")


class SandboxRenewResponse(BaseModel):
    """
    Response returned when renewing a sandbox expiration time.
    """

    expires_at: datetime = Field(
        description="The new absolute expiration time in UTC (RFC 3339 format).",
        alias="expires_at",
    )

    model_config = ConfigDict(populate_by_name=True)


class SandboxEndpoint(BaseModel):
    """
    Connection endpoint information for a sandbox.
    """

    endpoint: str = Field(description="Sandbox connection endpoint")
    headers: dict[str, str] = Field(
        default_factory=dict,
        description="Headers that must be included on every request targeting this endpoint (e.g. when the server requires them for routing or auth). Empty if not required.",
    )


class PaginationInfo(BaseModel):
    """
    Pagination metadata.
    """

    page: int = Field(description="Current page number (1-indexed)")
    page_size: int = Field(description="Number of items per page", alias="page_size")
    total_items: int = Field(
        description="Total number of items across all pages", alias="total_items"
    )
    total_pages: int = Field(description="Total number of pages", alias="total_pages")
    has_next_page: bool = Field(
        description="True if there is a next page available", alias="has_next_page"
    )

    model_config = ConfigDict(populate_by_name=True)


class PagedSandboxInfos(BaseModel):
    """
    A paginated list of sandbox information.
    """

    sandbox_infos: list[SandboxInfo] = Field(
        description="List of sandbox details for current page", alias="sandbox_infos"
    )
    pagination: PaginationInfo = Field(description="Pagination metadata")

    model_config = ConfigDict(populate_by_name=True)


class PagedSnapshotInfos(BaseModel):
    """
    A paginated list of snapshot information.
    """

    snapshot_infos: list[SnapshotInfo] = Field(
        description="List of snapshot details for current page", alias="snapshot_infos"
    )
    pagination: PaginationInfo = Field(description="Pagination metadata")

    model_config = ConfigDict(populate_by_name=True)


class SandboxFilter(BaseModel):
    """
    Filter criteria for listing sandboxes.
    """

    states: list[str] | None = Field(
        default=None, description="Filter by sandbox states"
    )
    metadata: dict[str, str] | None = Field(
        default=None, description="Filter by metadata key-value pairs"
    )
    page_size: int | None = Field(
        default=None, description="Number of items per page", alias="page_size"
    )
    page: int | None = Field(default=None, description="Page number (1-indexed)")

    @field_validator("page_size")
    @classmethod
    def page_size_must_be_positive(cls, v: int | None) -> int | None:
        if v is not None and v <= 0:
            raise ValueError("Page size must be positive")
        return v

    @field_validator("page")
    @classmethod
    def page_must_be_non_negative(cls, v: int | None) -> int | None:
        if v is not None and v < 0:
            raise ValueError("Page must be non-negative")
        return v

    model_config = ConfigDict(populate_by_name=True)


class SnapshotFilter(BaseModel):
    """
    Filter criteria for listing snapshots.
    """

    sandbox_id: str | None = Field(
        default=None,
        description="Filter by source sandbox id",
        alias="sandbox_id",
    )
    states: list[str] | None = Field(
        default=None, description="Filter by snapshot states"
    )
    page_size: int | None = Field(
        default=None, description="Number of items per page", alias="page_size"
    )
    page: int | None = Field(default=None, description="Page number (1-indexed)")

    @field_validator("page_size")
    @classmethod
    def snapshot_page_size_must_be_positive(cls, v: int | None) -> int | None:
        if v is not None and v <= 0:
            raise ValueError("Page size must be positive")
        return v

    @field_validator("page")
    @classmethod
    def snapshot_page_must_be_non_negative(cls, v: int | None) -> int | None:
        if v is not None and v < 0:
            raise ValueError("Page must be non-negative")
        return v

    model_config = ConfigDict(populate_by_name=True)


class SandboxMetrics(BaseModel):
    """
    Real-time resource usage metrics for a sandbox.
    """

    cpu_count: float = Field(
        description="Number of CPU cores available/allocated", alias="cpu_count"
    )
    cpu_used_percentage: float = Field(
        description="Current CPU usage as percentage (0.0 - 100.0)",
        alias="cpu_used_percentage",
    )
    memory_total_in_mib: float = Field(
        description="Total memory available in Mebibytes", alias="memory_total_in_mib"
    )
    memory_used_in_mib: float = Field(
        description="Memory currently used in Mebibytes", alias="memory_used_in_mib"
    )
    timestamp: int = Field(
        description="Timestamp of metric collection (Unix epoch milliseconds)"
    )

    model_config = ConfigDict(populate_by_name=True)


class SandboxState:
    """High-level lifecycle state of the sandbox.

    This class provides constant string values for sandbox states.
    Note that the sandbox service may introduce new states in future
    versions; clients should handle unknown string values gracefully.

    Common States:
        PENDING (str): Sandbox is being provisioned.
        RUNNING (str): Sandbox is running and ready to accept requests.
        PAUSING (str): Sandbox is in the process of pausing.
        PAUSED (str): Sandbox has been paused while retaining its state.
        STOPPING (str): Sandbox is being terminated.
        TERMINATED (str): Sandbox has been successfully terminated.
        FAILED (str): Sandbox encountered a critical error.
        UNKNOWN (str): State is unknown or unsupported by the current version.

    State Transitions:
        - Pending -> Running: After creation completes.
        - Running -> Pausing: When pause is requested.
        - Pausing -> Paused: After pause operation completes.
        - Paused -> Running: When resume is requested.
        - Running/Paused -> Stopping: When kill is requested or TTL expires.
        - Stopping -> Terminated: After kill/timeout operation completes.
        - Pending/Running/Paused -> Failed: On critical error.
    """

    PENDING = "Pending"
    RUNNING = "Running"
    PAUSING = "Pausing"
    PAUSED = "Paused"
    STOPPING = "Stopping"
    TERMINATED = "Terminated"
    FAILED = "Failed"
    UNKNOWN = "Unknown"

    @classmethod
    def values(cls) -> set[str]:
        """Returns a set of all known state values."""
        return {
            v for k, v in cls.__dict__.items()
            if k.isupper() and not k.startswith("_")
        }
