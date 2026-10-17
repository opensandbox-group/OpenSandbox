# Copyright 2025 The OpenSandbox Authors
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

import ipaddress
from datetime import datetime
from typing import Annotated, Dict, List, Literal, Optional

from pydantic import BaseModel, Field, PrivateAttr, RootModel, field_validator, model_validator

from opensandbox_server.constants import (
    IDLE_TIMEOUT_MAX_SECONDS,
    IDLE_TIMEOUT_MIN_SECONDS,
    PROBE_TIMEOUT_MAX_SECONDS,
    PROBE_TIMEOUT_MIN_SECONDS,
)
from opensandbox_server.constants import OPENSANDBOX_LIFECYCLE


def _is_ip_or_cidr(value: str) -> bool:
    """
    Mirror policy.go's target-kind detection (netip.ParseAddr/ParsePrefix): a
    target is IP/CIDR if it parses as either; anything else (including
    domains) is not.
    """
    try:
        ipaddress.ip_address(value)
        return True
    except ValueError:
        pass
    # netip.ParsePrefix rejects netmask notation and scoped prefixes; the
    # sidecar would classify both as domains, so mirror it.
    if "/" in value and (not value.partition("/")[2].isdigit() or "%" in value):
        return False
    try:
        ipaddress.ip_network(value, strict=False)
        return True
    except ValueError:
        return False

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
    Egress rule: allow/deny a domain, wildcard, IP/CIDR, and/or a set of TCP ports.
    """

    action: str = Field(..., description="Whether to allow or deny matching targets (allow | deny).")
    target: Optional[str] = Field(
        None,
        description=(
            "FQDN, wildcard domain (e.g., 'example.com', '*.example.com'), IP address, or CIDR. "
            "May be omitted when `ports` is set; otherwise required. Blank normalizes to omitted "
            "(the sidecar marshals port-only rules as \"target\": \"\" on read-back)."
        ),
    )
    ports: Optional[List[int]] = Field(
        None,
        min_length=1,
        max_length=256,
        description=(
            "Restricts this rule to specific TCP destination ports (1-65535). Omitted target with "
            "ports set applies to all destinations; a domain target with ports is rejected. TCP "
            "only, max 256 ports per rule."
        ),
    )

    @field_validator("target")
    @classmethod
    def normalize_target(cls, target: Optional[str]) -> Optional[str]:
        # Mirror policy.go: trim before classifying.
        if target is None:
            return None
        target = target.strip()
        return target or None

    @field_validator("ports")
    @classmethod
    def validate_ports(cls, ports: Optional[List[int]]) -> Optional[List[int]]:
        # Mirrors policy.go's normalizePorts.
        if ports is None:
            return ports
        seen: set[int] = set()
        for port in ports:
            if port < 1 or port > 65535:
                raise ValueError(f"NetworkRule port {port} out of range 1-65535.")
            if port in seen:
                raise ValueError(f"NetworkRule has duplicate port {port}.")
            seen.add(port)
        return ports

    @model_validator(mode="after")
    def validate_target_or_ports(self) -> "NetworkRule":
        if self.target is None and self.ports is None:
            raise ValueError("NetworkRule requires target, ports, or both.")
        if self.ports is not None and self.target is not None and not _is_ip_or_cidr(self.target):
            raise ValueError("NetworkRule ports are not supported for domain targets yet.")
        return self

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


class CredentialProxyConfig(BaseModel):
    """
    Credential proxy startup options.
    """

    enabled: bool = Field(
        False,
        description=(
            "When true, the server enables transparent MITM support required by "
            "Credential Vault injection. Plain egress network policy does not enable "
            "transparent MITM unless this option is set."
        ),
    )

    class Config:
        populate_by_name = True


# ============================================================================
# Credential Vault (server-routed; mirrors specs/egress-api.yaml sidecar shapes)
# ============================================================================


class InlineCredentialSource(BaseModel):
    type: Literal["inline"] = Field(..., description="Credential source type.")
    value: str = Field(
        ...,
        description="Write-only credential value; never returned on any read.",
    )

    class Config:
        populate_by_name = True
        extra = "forbid"


class Credential(BaseModel):
    name: str = Field(..., min_length=1, description="Unique credential name.")
    source: InlineCredentialSource = Field(..., description="Credential value source.")

    class Config:
        populate_by_name = True


class CredentialSubstitution(BaseModel):
    """Literal case-sensitive placeholder replacement for selected request surfaces."""

    credential: str = Field(..., min_length=1, description="Credential name used as the replacement value.")
    placeholder: str = Field(..., min_length=1, description="Literal placeholder to replace.")
    in_: list[Literal["path", "query", "header", "body"]] = Field(
        ...,
        min_length=1,
        alias="in",
        description="Request surfaces where the placeholder may be replaced.",
    )

    class Config:
        populate_by_name = True


class CustomHeaderEntry(BaseModel):
    name: str = Field(
        ...,
        pattern=r"^[A-Za-z0-9!#$%&'*+\-.^_`|~]+$",
        description="Header name injected by the binding.",
    )
    credential: str = Field(..., min_length=1, description="Credential name whose value is injected.")

    class Config:
        populate_by_name = True


class BearerCredentialAuth(BaseModel):
    type: Literal["bearer"]
    credential: str = Field(..., min_length=1, description="Credential name whose value becomes the bearer token.")
    substitutions: Optional[list[CredentialSubstitution]] = Field(
        default=None, description="Optional placeholder substitutions scoped to this binding."
    )

    class Config:
        populate_by_name = True


class BasicCredentialAuth(BaseModel):
    type: Literal["basic"]
    credential: str = Field(
        ...,
        min_length=1,
        description="Credential containing pre-encoded base64(username:password).",
    )
    substitutions: Optional[list[CredentialSubstitution]] = None

    class Config:
        populate_by_name = True


class ApiKeyCredentialAuth(BaseModel):
    type: Literal["apiKey"]
    name: str = Field(
        ...,
        pattern=r"^[A-Za-z0-9!#$%&'*+\-.^_`|~]+$",
        description="Header name receiving the API key.",
    )
    credential: str = Field(..., min_length=1, description="Credential name whose value is the API key.")
    substitutions: Optional[list[CredentialSubstitution]] = None

    class Config:
        populate_by_name = True


class CustomHeadersCredentialAuth(BaseModel):
    type: Literal["customHeaders"]
    headers: list[CustomHeaderEntry] = Field(..., min_length=1, description="Headers injected by this binding.")
    substitutions: Optional[list[CredentialSubstitution]] = None

    class Config:
        populate_by_name = True


class PassthroughCredentialAuth(BaseModel):
    type: Literal["passthrough"]
    substitutions: Optional[list[CredentialSubstitution]] = None

    class Config:
        populate_by_name = True


CredentialAuth = Annotated[
    BearerCredentialAuth
    | BasicCredentialAuth
    | ApiKeyCredentialAuth
    | CustomHeadersCredentialAuth
    | PassthroughCredentialAuth,
    Field(discriminator="type"),
]


class CredentialMatch(BaseModel):
    schemes: Optional[list[Literal["https", "http"]]] = Field(
        default=None, description="Schemes the binding matches; defaults to [https]."
    )
    ports: Optional[list[int]] = Field(
        default=None,
        deprecated=True,
        description="Deprecated. Port is derived from scheme; other values are rejected with a validation error.",
    )
    hosts: list[str] = Field(..., min_length=1, description="Host patterns (FQDN or wildcard domain).")
    methods: Optional[list[str]] = Field(
        default=None,
        description="HTTP methods the binding matches; defaults to GET, POST, PUT, PATCH, DELETE.",
    )
    paths: Optional[list[str]] = Field(
        default=None, description="Path patterns the binding matches; defaults to ['/*']."
    )

    class Config:
        populate_by_name = True


class CredentialBinding(BaseModel):
    name: str = Field(..., min_length=1, description="Unique binding name.")
    match: CredentialMatch = Field(..., description="Request matcher for credential injection.")
    auth: CredentialAuth = Field(..., description="Injected credential shape.")

    class Config:
        populate_by_name = True


class CredentialVaultCreateRequest(BaseModel):
    """Create the initial Credential Vault revision (create-only)."""

    credentials: list[Credential] = Field(..., description="Initial credentials (inline sources).")
    bindings: list[CredentialBinding] = Field(..., description="Initial credential bindings.")

    class Config:
        populate_by_name = True


class CredentialMutationSet(BaseModel):
    add: Optional[list[Credential]] = Field(
        default=None, description="Credentials to create; fails when a name already exists."
    )
    replace: Optional[list[Credential]] = Field(
        default=None, description="Credentials to replace; fails when a name does not exist."
    )
    delete: Optional[list[str]] = Field(
        default=None, description="Credential names to remove; fails when a name does not exist."
    )

    class Config:
        populate_by_name = True


class CredentialBindingMutationSet(BaseModel):
    add: Optional[list[CredentialBinding]] = Field(
        default=None, description="Bindings to create; fails when a name already exists."
    )
    replace: Optional[list[CredentialBinding]] = Field(
        default=None, description="Bindings to replace; fails when a name does not exist."
    )
    delete: Optional[list[str]] = Field(
        default=None, description="Binding names to remove; fails when a name does not exist."
    )

    class Config:
        populate_by_name = True


class CredentialVaultMutationRequest(BaseModel):
    """Atomically mutate credentials and bindings (server-side merge commit)."""

    expected_revision: Optional[int] = Field(
        default=None,
        alias="expectedRevision",
        description="Optional optimistic concurrency guard; mismatch fails with 412.",
    )
    credentials: Optional[CredentialMutationSet] = Field(
        default=None, description="Credential mutations (add/replace/delete)."
    )
    bindings: Optional[CredentialBindingMutationSet] = Field(
        default=None, description="Binding mutations (add/replace/delete)."
    )

    class Config:
        populate_by_name = True


class CredentialAuthMetadata(BaseModel):
    type: str = Field(..., description="Injected credential shape type.")
    name: Optional[str] = Field(
        default=None, description="Header name, for the apiKey shape only."
    )

    class Config:
        populate_by_name = True


class CredentialMetadata(BaseModel):
    name: str
    source_type: str = Field(..., alias="sourceType", description="Credential source type.")
    revision: int

    class Config:
        populate_by_name = True


class CredentialBindingMetadata(BaseModel):
    name: str
    revision: int
    match: Optional[CredentialMatch] = None
    auth: Optional[CredentialAuthMetadata] = None

    class Config:
        populate_by_name = True


class CredentialVaultState(BaseModel):
    """Sanitized Credential Vault state — credential values never appear."""

    revision: int
    credentials: list[CredentialMetadata]
    bindings: list[CredentialBindingMetadata]

    class Config:
        populate_by_name = True


class CredentialListResponse(BaseModel):
    revision: int
    credentials: list[CredentialMetadata]

    class Config:
        populate_by_name = True


class CredentialBindingListResponse(BaseModel):
    revision: int
    bindings: list[CredentialBindingMetadata]

    class Config:
        populate_by_name = True


class LifecycleHook(BaseModel):
    """Command executed by execd before the user entrypoint starts."""

    command: List[str] = Field(..., min_length=1)
    timeout_seconds: Optional[int] = Field(None, alias="timeoutSeconds", ge=1, le=10800)

    @model_validator(mode="after")
    def validate_command(self) -> "LifecycleHook":
        if not self.command[0].strip():
            raise ValueError("Lifecycle hook command must not be empty.")
        return self

    class Config:
        populate_by_name = True
        extra = "forbid"


class PeriodicLifecycleHook(BaseModel):
    """Named command scheduled by execd while the sandbox is running."""

    name: str = Field(..., min_length=1)
    schedule: str = Field(..., min_length=1)
    command: List[str] = Field(..., min_length=1)
    timeout_seconds: Optional[int] = Field(None, alias="timeoutSeconds", ge=1, le=300)

    @model_validator(mode="after")
    def normalize_and_validate(self) -> "PeriodicLifecycleHook":
        self.name = self.name.strip()
        self.schedule = self.schedule.strip()
        if not self.name:
            raise ValueError("Periodic lifecycle hook name must not be blank.")
        if not self.schedule:
            raise ValueError("Periodic lifecycle hook schedule must not be blank.")
        if not self.command[0].strip():
            raise ValueError("Periodic lifecycle hook command must not be empty.")
        return self

    class Config:
        populate_by_name = True
        extra = "forbid"


class ActivityProbe(BaseModel):
    """App-declared activity probe (``idlePolicy.activeProbe``, OSEP-0024).

    ``GET {path}`` on ``{port}`` answering ``200 {"active": true}`` counts
    as activity and resets the idle clock.
    """

    port: int = Field(..., ge=1, le=65535)
    path: str = Field(..., min_length=1, max_length=512)
    timeout_seconds: Optional[int] = Field(
        None,
        alias="timeoutSeconds",
        ge=PROBE_TIMEOUT_MIN_SECONDS,
        le=PROBE_TIMEOUT_MAX_SECONDS,
    )

    @model_validator(mode="after")
    def validate_path(self) -> "ActivityProbe":
        if not self.path.startswith("/"):
            raise ValueError("idlePolicy.activeProbe.path must be an absolute HTTP path.")
        if any(ord(char) < 0x20 or char == "\x7f" or char.isspace() for char in self.path):
            raise ValueError(
                "idlePolicy.activeProbe.path must not contain whitespace or control characters."
            )
        return self

    class Config:
        populate_by_name = True
        extra = "forbid"


class IdlePolicy(BaseModel):
    """Idle behavior declared by the sandbox (OSEP-0024).

    With a timeout the sandbox is auto-paused after X idle seconds; without
    it the policy is wake-only. Omitting ``idlePolicy`` preserves today's
    behavior exactly.
    """

    idle_timeout_seconds: Optional[int] = Field(
        None,
        alias="idleTimeoutSeconds",
        ge=IDLE_TIMEOUT_MIN_SECONDS,
        le=IDLE_TIMEOUT_MAX_SECONDS,
        description="Idle threshold X in seconds. Absent means a wake-only policy.",
    )
    on_idle: Optional[Literal["pause"]] = Field(
        None,
        alias="onIdle",
        description="Action taken at the idle threshold; only 'pause' exists today.",
    )
    wake_on_access: bool = Field(
        True,
        alias="wakeOnAccess",
        description=(
            "Whether traffic may wake the sandbox from Paused. False keeps "
            "today's manual-resume-only behavior."
        ),
    )
    active_probe: Optional[ActivityProbe] = Field(None, alias="activeProbe")

    @model_validator(mode="after")
    def validate_pairing(self) -> "IdlePolicy":
        if self.idle_timeout_seconds is None:
            if self.on_idle is not None:
                raise ValueError(
                    "idlePolicy.onIdle must be omitted when idleTimeoutSeconds is omitted "
                    "(a policy without a timeout is wake-only)."
                )
            if self.active_probe is not None:
                raise ValueError(
                    "idlePolicy.activeProbe requires idleTimeoutSeconds; the probe "
                    "governs auto-pause and a wake-only policy is never probed."
                )
        elif self.on_idle is None:
            raise ValueError(
                "idlePolicy.onIdle is required when idleTimeoutSeconds is set."
            )
        return self

    class Config:
        populate_by_name = True
        extra = "forbid"


class SandboxLifecycle(BaseModel):
    """Extensible lifecycle configuration: hooks run in the guest via execd;
    ``idlePolicy`` is control-plane state (OSEP-0024) and never reaches it.
    """

    pre_start: Optional[LifecycleHook] = Field(None, alias="preStart")
    periodic: Optional[List[PeriodicLifecycleHook]] = None
    idle_policy: Optional[IdlePolicy] = Field(
        None,
        alias="idlePolicy",
        description="Auto-pause and wake-on-access behavior (fast-sandbox backend only).",
    )

    @property
    def has_hooks(self) -> bool:
        return self.pre_start is not None or bool(self.periodic)

    @model_validator(mode="after")
    def validate_periodic_names(self) -> "SandboxLifecycle":
        names = [hook.name for hook in self.periodic or []]
        if len(names) != len(set(names)):
            raise ValueError("Periodic lifecycle hook names must be unique.")
        return self

    class Config:
        populate_by_name = True
        extra = "forbid"


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
            "on this request; pre-existing volumes are never removed. For "
            "Kubernetes, the resulting PVC delete then triggers the bound PV's "
            "StorageClass reclaim policy (Retain/Delete)."
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

    # Internal routing hint: set by snapshot restore resolution to the backend
    # that produced the snapshot (e.g. "fsb"); never serialized on the wire.
    _resolved_snapshot_backend: Optional[str] = PrivateAttr(default=None)

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
        description="Runtime resource constraints (hard caps) for the sandbox instance. Optional when poolRef is provided.",
    )
    resource_requests: Optional[ResourceLimits] = Field(
        None,
        alias="resourceRequests",
        description=(
            "Resource reservations (guaranteed minimums) for the sandbox instance. "
            "When provided, used as Kubernetes resource requests, enabling Burstable QoS. "
            "When omitted, resourceLimits values are used for both limits and requests."
        ),
    )
    env: Optional[Dict[str, Optional[str]]] = Field(
        None,
        description="Environment variables to inject into the sandbox runtime",
    )
    metadata: Optional[Dict[str, str]] = Field(
        None,
        description="Custom key-value metadata for management, filtering, and tagging",
    )
    lifecycle: Optional[SandboxLifecycle] = Field(
        None,
        description="Optional declarative lifecycle hooks executed by execd.",
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
            "Empty/omitted means allow-all until updated. Not supported together with "
            "extensions.poolRef because pooled pods are pre-created."
        ),
    )
    credential_proxy: Optional[CredentialProxyConfig] = Field(
        None,
        alias="credentialProxy",
        description=(
            "Optional Credential Vault proxy startup settings. Set enabled=true to "
            "enable transparent MITM support for credential injection."
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
    template_id: Optional[str] = Field(
        None,
        alias="templateId",
        min_length=1,
        description=(
            "Fsb template to create the sandbox from. Mutually "
            "exclusive with image and snapshotId; in template mode the workload "
            "shape is fixed by the template's golden image, so entrypoint, env, "
            "resourceLimits, resourceRequests, volumes, platform, credentialProxy, "
            "secureAccess and lifecycle hooks are rejected. lifecycle.idlePolicy "
            "is allowed (control-plane state, not workload shape). timeout is "
            "required."
        ),
    )

    @model_validator(mode="after")
    def validate_source_and_entrypoint(self) -> "CreateSandboxRequest":
        if self.env and OPENSANDBOX_LIFECYCLE in self.env:
            raise ValueError(
                f"Environment variable '{OPENSANDBOX_LIFECYCLE}' is reserved. "
                "Use the lifecycle request field instead."
            )

        # Template mode (fsb): workload-shaping fields are rejected, not
        # ignored; idlePolicy is control-plane state, so a hooks-free
        # lifecycle may carry it.
        if self.template_id is not None and self.template_id.strip():
            conflicts = {
                "image": self.image is not None,
                "snapshotId": bool((self.snapshot_id or "").strip()),
                "entrypoint": self.entrypoint is not None,
                "env": self.env is not None,
                "resourceLimits": self.resource_limits is not None,
                "resourceRequests": self.resource_requests is not None,
                "volumes": self.volumes is not None,
                "platform": self.platform is not None,
                "credentialProxy": self.credential_proxy is not None,
                "secureAccess": self.secure_access,
                "lifecycle": self.lifecycle is not None and self.lifecycle.has_hooks,
            }
            present = [name for name, is_set in conflicts.items() if is_set]
            if present:
                raise ValueError(
                    f"templateId cannot be combined with: {', '.join(sorted(present))}; "
                    "the workload shape is fixed by the template's golden image."
                )
            if self.timeout is None:
                raise ValueError("timeout is required when templateId is provided.")
            return self
        if self.template_id is not None:
            self.template_id = None  # normalize blank templateId

        # When poolRef is set, image/snapshotId/entrypoint/resourceLimits are
        # all defined in the Pool CRD and not required from the caller.
        has_pool_ref = bool((self.extensions or {}).get("poolRef", "").strip())
        if has_pool_ref:
            if self.lifecycle is not None and self.lifecycle.has_hooks:
                raise ValueError(
                    "lifecycle hooks cannot be used together with poolRef; "
                    "lifecycle.idlePolicy is not a hook and is allowed where "
                    "the backend supports it."
                )
            # Reject conflicting fields that would be ignored in pool mode
            if bool((self.snapshot_id or "").strip()):
                raise ValueError("snapshotId cannot be used together with poolRef.")
            # Normalize blank snapshotId so downstream code won't see
            # a truthy whitespace string (e.g. "   ") as a real value.
            if self.snapshot_id is not None and not self.snapshot_id.strip():
                self.snapshot_id = None
            return self

        if self.credential_proxy and self.credential_proxy.enabled:
            if self.network_policy is None:
                raise ValueError("credentialProxy.enabled requires networkPolicy.")

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

    @property
    def resolved_snapshot_backend(self) -> Optional[str]:
        """Backend that produced the snapshot being restored, if resolved."""
        return self._resolved_snapshot_backend


class CreateSandboxResponse(BaseModel):
    """
    Response from creating a new sandbox.

    Contains essential information without image and updatedAt.
    """
    id: str = Field(..., description="Unique sandbox identifier")
    status: SandboxStatus = Field(..., description="Current lifecycle status and detailed state information")
    metadata: Optional[Dict[str, str]] = Field(None, description="Custom metadata from creation request")
    extensions: Optional[Dict[str, str]] = Field(
        None,
        description="Opaque extension data restored from provider-specific storage",
    )
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


class AllocationSummary(BaseModel):
    """Current runtime-confirmed pool allocation summary."""
    mode: Literal["pool"] = Field("pool", description="Allocation mode.")
    pool_ref: str = Field(..., alias="poolRef", description="Concrete pool reference currently allocated.")
    state: Literal["allocated"] = Field("allocated", description="Current confirmed allocation state.")

    class Config:
        populate_by_name = True
        extra = "forbid"


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
    extensions: Optional[Dict[str, str]] = Field(
        None,
        description="Opaque extension data restored from provider-specific storage",
    )
    allocation: Optional[AllocationSummary] = Field(
        None,
        description=(
            "Current runtime-confirmed pool allocation summary. Omitted unless the runtime confirms "
            "an active pool allocation; it is not a request echo, allocation history, readiness signal, "
            "or Kubernetes introspection result."
        ),
    )
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
    name: Optional[str] = Field(
        None,
        description="Filter snapshots by exact snapshot name",
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
        description=(
            "New absolute expiration time in UTC (RFC 3339 format). Must be in the future. "
            "May be earlier than, equal to, or later than the current expiresAt. "
            "The creation-time limit server.max_sandbox_timeout_seconds does not apply."
        ),
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


# ============================================================================
# Fsb Templates
# ============================================================================

class FsbTemplateReadiness(BaseModel):
    """
    Build-side readiness gate for a fsb template.
    """
    probe: Optional[str] = Field(
        None,
        description=(
            "Readiness probe checked first during the golden-image build; "
            "e.g. 'tcp://127.0.0.1:44772' or 'cmd://<command>'."
        ),
    )
    warmup_seconds: Optional[int] = Field(
        None,
        ge=0,
        alias="warmupSeconds",
        description="Fallback warmup window in seconds (default 60).",
    )

    class Config:
        populate_by_name = True


class CreateFsbTemplateRequest(BaseModel):
    """
    Request to create a fsb template: a fast-sandbox golden-image build.

    The server persists the build intent, resolves it to a SandboxTemplate
    CRD, and reports the asynchronous build through the template status.
    Kernel, execd and guest init are server-side build inputs supplied from
    the [kubernetes] configuration, not client fields.
    """
    image: str = Field(
        ...,
        min_length=1,
        description="Source OCI image reference the golden image is built from",
    )
    resource_limits: Optional[ResourceLimits] = Field(
        None,
        alias="resourceLimits",
        description=(
            "Guest machine sizing: cpu -> guest vCPUs, memory -> guest memory, "
            "disk -> logical size of the guest rootfs. The artifact set is "
            "stored and P2P-pulled at the disk size, so keep it just above "
            "the expanded source image. Defaults: cpu 1, memory 512Mi, "
            "disk 2Gi."
        ),
    )
    entrypoint: Optional[List[str]] = Field(
        None,
        min_length=1,
        description="Guest business command (argv); empty defaults to ['tail', '-f', '/dev/null'].",
    )
    env: Optional[Dict[str, str]] = Field(
        None,
        description=(
            "Environment variables baked into the golden image (injected as "
            "/etc/sandbox-init.env in the guest; literal values only). The "
            "source image's own OCI Config.Env is inherited like a container "
            "runtime would; an env with the same name here overrides the "
            "inherited value. Names must be valid shell variable names "
            "([A-Za-z_][A-Za-z0-9_]*)."
        ),
    )
    metadata: Optional[Dict[str, str]] = Field(
        None,
        description="Custom key-value metadata for management, filtering, and tagging",
    )
    readiness: Optional[FsbTemplateReadiness] = Field(
        None,
        description="Optional build-side readiness gate",
    )
    publish: str = Field(
        ...,
        min_length=1,
        description="S3-compatible publish target for the built artifacts, e.g. 's3://bucket/publish'",
    )
    format: Literal["native", "overlaybd"] = Field(
        "overlaybd",
        description="Storage encoding of the produced snapshot set",
    )

    class Config:
        populate_by_name = True
        extra = "forbid"


class FsbTemplateStatus(BaseModel):
    """
    Status of a fsb template build.
    """
    phase: Literal["Pending", "Building", "Succeeded", "Failed"] = Field(
        ...,
        description="Build lifecycle phase",
    )
    manifest_ref: Optional[str] = Field(
        None,
        alias="manifestRef",
        description="S3 manifest reference of the published artifacts; present when Succeeded",
    )
    message: Optional[str] = Field(
        None,
        description="Failure reason when phase is Failed",
    )

    class Config:
        populate_by_name = True


class FsbTemplate(BaseModel):
    """
    A fsb template: a golden image whose build is declared and executed
    by fast-sandbox.
    """
    template_id: str = Field(..., alias="templateId", description="Server-generated template ID (tpl_<uuid>)")
    image: str = Field(..., description="Source OCI image reference")
    resource_limits: Optional[ResourceLimits] = Field(None, alias="resourceLimits")
    entrypoint: Optional[List[str]] = Field(None, description="Guest business command (argv)")
    env: Optional[Dict[str, str]] = Field(
        None, description="Environment variables baked into the golden image"
    )
    metadata: Optional[Dict[str, str]] = Field(None, description="Custom metadata from the creation request")
    readiness: Optional[FsbTemplateReadiness] = Field(None, description="Build-side readiness gate")
    publish: str = Field(..., description="S3-compatible publish target")
    format: Literal["native", "overlaybd"] = Field(..., description="Snapshot storage encoding")
    status: FsbTemplateStatus = Field(..., description="Build status")
    created_at: datetime = Field(..., alias="createdAt", description="Creation timestamp")
    updated_at: datetime = Field(..., alias="updatedAt", description="Last update timestamp")

    class Config:
        populate_by_name = True


class ListFsbTemplatesResponse(BaseModel):
    """
    Paginated collection of fsb templates.
    """
    items: List[FsbTemplate] = Field(..., description="List of templates")
    pagination: PaginationInfo = Field(..., description="Pagination metadata")


# ============================================================================
# Metrics
# ============================================================================

class MetricsEvent(BaseModel):
    """
    SDK-reported metrics event (Phase 1: sandbox creation latency).
    """

    event_type: Literal["sandbox.create"] = Field(
        ...,
        alias="eventType",
        description="Metric event type",
    )
    sandbox_id: Optional[str] = Field(
        default=None,
        alias="sandboxId",
        description="Sandbox identifier when available",
    )
    image: Optional[str] = Field(
        default=None,
        description="Container image URI or snapshot startup source label",
    )
    create_duration_ms: int = Field(
        ...,
        alias="createDurationMs",
        ge=0,
        description="Wall-clock duration in milliseconds from create start to ready or failure",
    )
    success: bool = Field(
        ...,
        description="Whether create + readiness completed successfully",
    )

    class Config:
        populate_by_name = True
