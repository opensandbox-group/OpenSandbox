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
Application configuration management for sandbox server.

Loads configuration from a TOML file (default: ~/.sandbox.toml) and exposes
helpers to access the parsed settings throughout the application.
"""

from __future__ import annotations

import base64
import ipaddress
import logging
import os
import re
from pathlib import Path
from typing import Any, ClassVar, Dict, Literal, Optional

from pydantic import BaseModel, Field, ValidationError, field_validator, model_validator

try:  # Python 3.11+
    import tomllib  # type: ignore[attr-defined]
except ModuleNotFoundError:  # Python 3.10 fallback
    import tomli as tomllib  # type: ignore[import]

logger = logging.getLogger(__name__)

CONFIG_ENV_VAR = "SANDBOX_CONFIG_PATH"
DEFAULT_CONFIG_PATH = Path.home() / ".sandbox.toml"

API_KEY_ENV_VAR = "OPENSANDBOX_SERVER_API_KEY"

_HOSTNAME_RE = re.compile(r"^(?=.{1,253}$)(?!-)[A-Za-z0-9-]{1,63}(?:\.(?!-)[A-Za-z0-9-]{1,63})*$")
_WILDCARD_DOMAIN_RE = re.compile(r"^\*\.(?!-)[A-Za-z0-9-]{1,63}(?:\.[A-Za-z0-9-]{1,63})+$")
_IPV4_WITH_PORT_RE = re.compile(r"^(?P<ip>(?:\d{1,3}\.){3}\d{1,3})(?::(?P<port>\d{1,5}))?$")

INGRESS_MODE_DIRECT = "direct"
INGRESS_MODE_GATEWAY = "gateway"
GATEWAY_ROUTE_MODE_WILDCARD = "wildcard"
GATEWAY_ROUTE_MODE_HEADER = "header"
GATEWAY_ROUTE_MODE_URI = "uri"

EGRESS_MODE_DNS = "dns"
EGRESS_MODE_DNS_NFT = "dns+nft"


def _is_valid_ip(host: str) -> bool:
    try:
        ipaddress.ip_address(host)
        return True
    except ValueError:
        return False


def _is_valid_ip_or_ip_port(address: str) -> bool:
    match = _IPV4_WITH_PORT_RE.match(address)
    if not match:
        return False
    ip_str = match.group("ip")
    if not _is_valid_ip(ip_str):
        return False
    port_str = match.group("port")
    if port_str is None:
        return True
    try:
        port = int(port_str)
    except ValueError:
        return False
    return 1 <= port <= 65535


def _is_valid_hostname(address: str) -> bool:
    host = address
    port_str: Optional[str] = None

    if ":" in address:
        host, _, port_str = address.rpartition(":")
        if not host or not port_str:
            return False
        if not port_str.isdigit():
            return False
        port = int(port_str)
        if not (1 <= port <= 65535):
            return False

    return bool(_HOSTNAME_RE.match(host))


def _is_wildcard_domain(host: str) -> bool:
    return bool(_WILDCARD_DOMAIN_RE.match(host))


class RenewIntentRedisConfig(BaseModel):
    """🧪 [EXPERIMENTAL] Redis list consumer for renew-intent queue (ingress gateway path)."""

    enabled: bool = Field(
        default=False,
        description=(
            "🧪 [EXPERIMENTAL] When true, server workers consume renew intents from Redis "
            "(ingress gateway path)."
        ),
    )
    dsn: Optional[str] = Field(
        default=None,
        description=(
            '🧪 [EXPERIMENTAL] Redis DSN (e.g. "redis://127.0.0.1:6379/0"). '
            "Required when redis.enabled is true."
        ),
    )
    queue_key: str = Field(
        default="opensandbox:renew:intent",
        min_length=1,
        description="🧪 [EXPERIMENTAL] Redis List key for LPUSH/BRPOP renew-intent JSON payloads.",
    )
    consumer_concurrency: int = Field(
        default=8,
        ge=1,
        description="🧪 [EXPERIMENTAL] Number of concurrent BRPOP worker tasks.",
    )

    @model_validator(mode="after")
    def require_dsn_when_redis_enabled(self) -> "RenewIntentRedisConfig":
        if self.enabled and (self.dsn is None or not str(self.dsn).strip()):
            raise ValueError(
                "[renew_intent] redis.dsn must be set when redis.enabled is true."
            )
        return self


class RenewIntentConfig(BaseModel):
    """🧪 [EXPERIMENTAL] Renew sandbox expiration when access is observed (proxy and/or Redis queue)."""

    enabled: bool = Field(
        default=False,
        description=(
            "🧪 [EXPERIMENTAL] Master switch for auto-renew on reverse-proxy access and/or Redis "
            "ingress intents. When false, renew-intent logic is off."
        ),
    )
    min_interval_seconds: int = Field(
        default=60,
        ge=1,
        description=(
            "🧪 [EXPERIMENTAL] Minimum seconds between successful renewals for the same sandbox "
            "(cooldown)."
        ),
    )
    redis: RenewIntentRedisConfig = Field(
        default_factory=RenewIntentRedisConfig,
        description=(
            "🧪 [EXPERIMENTAL] Redis queue consumer for ingress gateway renew-intent mode. "
            "In TOML, set keys under the same [renew_intent] table as redis.enabled, "
            "redis.dsn, redis.queue_key, redis.consumer_concurrency (dotted keys)."
        ),
    )


_KEY_ID_RE = re.compile(r"^[0-9a-z]$")


def _try_decode_base64(s: str) -> bytes | None:
    """Accept both padded and unpadded base64."""
    try:
        return base64.b64decode(s, validate=True)
    except Exception:
        pass
    try:
        padded = s + "=" * ((4 - len(s) % 4) % 4)
        return base64.b64decode(padded, validate=True)
    except Exception:
        return None


class SecureAccessKey(BaseModel):
    """A signing key entry for OSEP-0011 signed route verification."""

    key_id: str = Field(
        ...,
        min_length=1,
        max_length=1,
        description="Key identifier, exactly one character in ``[0-9a-z]``.",
    )
    key: str = Field(
        ...,
        min_length=1,
        description="Base64-encoded signing key secret bytes.",
    )

    @field_validator("key_id")
    @classmethod
    def validate_key_id_char(cls, v: str) -> str:
        if not _KEY_ID_RE.match(v):
            raise ValueError(
                f"key_id must be exactly one character [0-9a-z], got {v!r}"
            )
        return v

    @field_validator("key")
    @classmethod
    def validate_key_is_base64(cls, v: str) -> str:
        decoded = _try_decode_base64(v)
        if decoded is None:
            raise ValueError(f"key is not valid base64: {v!r}")
        if not decoded:
            raise ValueError("key must decode to at least 1 byte")
        return v

    def get_secret_bytes(self) -> bytes:
        decoded = _try_decode_base64(self.key)
        assert decoded is not None, "key was validated at construction"
        return decoded


class SecureAccessConfig(BaseModel):
    """OSEP-0011 secure access signing configuration."""

    active_key: str = Field(
        ...,
        min_length=1,
        max_length=1,
        description=(
            "Identifier of the active signing key. Must reference a ``key_id`` "
            "present in ``keys``. Exactly one character ``[0-9a-z]``."
        ),
    )
    keys: list[SecureAccessKey] = Field(
        ...,
        min_length=1,
        description="List of signing keys available for route signing and verification.",
    )

    @field_validator("active_key")
    @classmethod
    def validate_active_key_char(cls, v: str) -> str:
        if not _KEY_ID_RE.match(v):
            raise ValueError(
                f"active_key must be exactly one character [0-9a-z], got {v!r}"
            )
        return v

    @model_validator(mode="after")
    def ensure_active_key_exists(self) -> "SecureAccessConfig":
        seen = set()
        for k in self.keys:
            if k.key_id in seen:
                raise ValueError(
                    f"duplicate secure_access key_id {k.key_id!r}; "
                    "each key_id must be unique"
                )
            seen.add(k.key_id)
        if self.active_key not in seen:
            raise ValueError(
                f"active_key {self.active_key!r} not found in secure_access.keys; "
                f"available keys: {sorted(seen)}"
            )
        return self

    def get_active_secret_bytes(self) -> bytes:
        for k in self.keys:
            if k.key_id == self.active_key:
                return k.get_secret_bytes()
        raise RuntimeError(
            f"active_key {self.active_key!r} not in keys list"
        )


class GatewayRouteModeConfig(BaseModel):
    """Routing strategy for gateway ingress exposure."""

    mode: Literal[
        GATEWAY_ROUTE_MODE_WILDCARD,
        GATEWAY_ROUTE_MODE_HEADER,
        GATEWAY_ROUTE_MODE_URI,
    ] = Field(
        ...,
        description="Routing mode used by the gateway (wildcard, header, uri).",
    )

    class Config:
        populate_by_name = True


class GatewayConfig(BaseModel):
    """Gateway mode configuration for ingress exposure."""

    address: str = Field(
        ...,
        description="Gateway host used to expose sandboxes (domain or IP, may include :port; scheme is not allowed).",
        min_length=1,
    )
    route: GatewayRouteModeConfig = Field(
        ...,
        description="Routing mode configuration used by the gateway.",
    )


class IngressConfig(BaseModel):
    """Configuration for exposing sandbox ingress."""

    mode: Literal[INGRESS_MODE_DIRECT, INGRESS_MODE_GATEWAY] = Field(
        default=INGRESS_MODE_DIRECT,
        description="Ingress exposure mode (direct or gateway).",
    )
    gateway: Optional[GatewayConfig] = Field(
        default=None,
        description="Gateway configuration required when mode = 'gateway'.",
    )
    secure_access: Optional[SecureAccessConfig] = Field(
        default=None,
        description=(
            "OSEP-0011 secure access signing configuration. "
            "When set, the server can issue signed route tokens and static "
            "SecureAccessTokens for sandbox endpoints. "
            "Requires ingress.mode = 'gateway'."
        ),
    )

    @model_validator(mode="after")
    def validate_ingress_mode(self) -> "IngressConfig":
        if self.mode == INGRESS_MODE_GATEWAY and self.gateway is None:
            raise ValueError("gateway block must be provided when ingress.mode = 'gateway'.")
        if self.mode == INGRESS_MODE_DIRECT and self.gateway is not None:
            raise ValueError("gateway block must be omitted unless ingress.mode = 'gateway'.")

        if self.secure_access is not None and self.mode != INGRESS_MODE_GATEWAY:
            raise ValueError(
                "secure_access block requires ingress.mode = 'gateway'."
            )

        if self.mode == INGRESS_MODE_GATEWAY and self.gateway:
            route_mode = self.gateway.route.mode
            address_raw = self.gateway.address
            hostport = address_raw
            if "://" in address_raw:
                raise ValueError("ingress.gateway.address must not include a scheme; clients choose http/https.")

            if route_mode == GATEWAY_ROUTE_MODE_WILDCARD:
                if not _is_wildcard_domain(hostport):
                    raise ValueError(
                        "ingress.gateway.address must be a wildcard domain (e.g., *.example.com) "
                        "when gateway.route.mode is wildcard."
                    )
            else:
                if "*" in hostport:
                    raise ValueError(
                        "ingress.gateway.address must not contain wildcard when gateway.route.mode is not wildcard."
                    )
                if route_mode == GATEWAY_ROUTE_MODE_HEADER:
                    if not (_is_valid_hostname(hostport) or _is_valid_ip_or_ip_port(hostport)):
                        raise ValueError(
                            "ingress.gateway.address must be a valid hostname, hostname:port, IP, or IP:port "
                            "when gateway.route.mode is header."
                        )
                elif route_mode == GATEWAY_ROUTE_MODE_URI:
                    if not hostport.strip():
                        raise ValueError(
                            "ingress.gateway.address must not be empty when gateway.route.mode is uri."
                        )
        return self


class LogConfig(BaseModel):
    """Logging configuration."""

    level: str = Field(
        default="INFO",
        description="Python logging level for the server process.",
        min_length=3,
    )
    file_enabled: bool = Field(
        default=False,
        description=(
            "When true, logs are written to rotating files instead of stdout. "
            "Uses default paths (/var/log/opensandbox/) unless file_path/access_file_path are set."
        ),
    )
    file_path: Optional[str] = Field(
        default=None,
        description=(
            "Path to the main log file. When file_enabled=true and this is unset, "
            "defaults to ~/logs/opensandbox/server.log."
        ),
    )
    access_file_path: Optional[str] = Field(
        default=None,
        description=(
            "Path to the HTTP access log file. When file_enabled=true, access logs are written "
            "to a separate file by default (~/logs/opensandbox/access.log). Set this to override "
            "the path. Example: '~/logs/opensandbox/access.log'."
        ),
    )
    file_max_bytes: int = Field(
        default=100 * 1024 * 1024,  # 100MB
        ge=1,
        description="Maximum size of each log file in bytes before rotation (default: 100MB).",
    )
    file_backup_count: int = Field(
        default=5,
        ge=0,
        description="Number of backup log files to keep after rotation (default: 5).",
    )

    # Default paths when file_enabled=true and user paths are not set.
    # Uses ~/logs/opensandbox/ which is writable for non-root users.
    DEFAULT_FILE_PATH: ClassVar[str] = str(Path.home() / "logs" / "opensandbox" / "server.log")
    DEFAULT_ACCESS_FILE_PATH: ClassVar[str] = str(Path.home() / "logs" / "opensandbox" / "access.log")

    def resolved_file_path(self) -> Optional[str]:
        """Return the effective file path, using default if file_enabled and not overridden."""
        if not self.file_enabled:
            return None
        return self.file_path or self.DEFAULT_FILE_PATH

    def resolved_access_file_path(self) -> Optional[str]:
        """Return the effective access file path (defaults to separate file when file_enabled)."""
        if not self.file_enabled:
            return None
        return self.access_file_path or self.DEFAULT_ACCESS_FILE_PATH


class ServerConfig(BaseModel):
    """FastAPI server configuration."""

    host: str = Field(
        default="0.0.0.0",
        description="Interface bound by the lifecycle API server.",
        min_length=1,
    )
    port: int = Field(
        default=8080,
        ge=1,
        le=65535,
        description="Port exposed by the lifecycle API server.",
    )
    timeout_keep_alive: int = Field(
        default=30,
        ge=1,
        description=(
            "Idle keep-alive timeout in seconds passed to uvicorn. "
            "Connections idle longer than this may be closed by the server."
        ),
    )
    limit_concurrency: Optional[int] = Field(
        default=1024,
        ge=0,
        description=(
            "Maximum concurrent connections before returning 503. "
            "Set to 0 to disable (TOML cannot express null). "
            "Provides backpressure protection under burst load."
        ),
    )

    @field_validator("limit_concurrency", mode="after")
    @classmethod
    def _zero_disables_limit_concurrency(cls, value: Optional[int]) -> Optional[int]:
        # Translate the TOML-friendly sentinel 0 into None so uvicorn applies
        # no concurrency cap. TOML has no null literal, so 0 is the only way
        # to disable the limit from the config file.
        return None if value == 0 else value
    backlog: int = Field(
        default=2048,
        ge=1,
        description="Socket listen backlog passed to uvicorn.",
    )
    thread_pool_size: int = Field(
        default=200,
        ge=1,
        description=(
            "Maximum size of the anyio default threadpool used by FastAPI "
            "to run sync route handlers. Default anyio limit is 40, which "
            "throttles bursts of blocking sandbox list/get/delete operations "
            "under high concurrency."
        ),
    )
    loop: Literal["auto", "uvloop", "asyncio"] = Field(
        default="auto",
        description=(
            "Event loop implementation. 'auto' uses uvloop when available and "
            "falls back to asyncio. 'asyncio' forces the stdlib loop."
        ),
    )
    http: Literal["auto", "httptools", "h11"] = Field(
        default="auto",
        description=(
            "HTTP protocol parser. 'auto' uses httptools when available and "
            "falls back to h11."
        ),
    )
    api_key: Optional[str] = Field(
        default=None,
        description="Global API key for authenticating incoming lifecycle API calls.",
    )
    eip: Optional[str] = Field(
        default=None,
        description="Bound public IP. When set, used as the host part when returning sandbox endpoints.",
    )
    max_sandbox_timeout_seconds: Optional[int] = Field(
        default=None,
        ge=60,
        description=(
            "Maximum allowed sandbox TTL in seconds for requests that specify timeout. "
            "Omit from config to disable the server-side upper bound."
        ),
    )


class KubernetesRuntimeConfig(BaseModel):
    """Kubernetes-specific runtime configuration."""

    kubeconfig_path: Optional[str] = Field(
        default=None,
        description="Absolute path to the kubeconfig file used for API authentication.",
    )
    informer_enabled: bool = Field(
        default=True,
        description=(
            "[Beta] Enable informer-backed cache for workload reads. "
            "Keeps a watch to reduce API pressure; set false to disable."
        ),
    )
    informer_resync_seconds: int = Field(
        default=300,
        ge=1,
        description=(
            "[Beta] Full resync interval for informer cache (seconds). "
            "Shorter intervals refresh the cache more eagerly."
        ),
    )
    informer_watch_timeout_seconds: int = Field(
        default=60,
        ge=1,
        description=(
            "[Beta] Watch timeout (seconds) before restarting the informer stream."
        ),
    )
    read_qps: float = Field(
        default=0.0,
        ge=0,
        description=(
            "Maximum read requests per second to the Kubernetes API (get/list). "
            "0 means unlimited (no rate limiting)."
        ),
    )
    read_burst: int = Field(
        default=0,
        ge=0,
        description=(
            "Burst size for the read rate limiter. "
            "0 means use read_qps as burst (minimum 1)."
        ),
    )
    write_qps: float = Field(
        default=0.0,
        ge=0,
        description=(
            "Maximum write requests per second to the Kubernetes API (create/delete/patch). "
            "0 means unlimited (no rate limiting)."
        ),
    )
    write_burst: int = Field(
        default=0,
        ge=0,
        description=(
            "Burst size for the write rate limiter. "
            "0 means use write_qps as burst (minimum 1)."
        ),
    )
    namespace: Optional[str] = Field(
        default=None,
        description="Namespace used for sandbox workloads.",
    )
    service_account: Optional[str] = Field(
        default=None,
        description="Service account bound to sandbox workloads.",
    )
    workload_provider: Optional[str] = Field(
        default=None,
        description="Workload provider type. If not specified, uses the first registered provider.",
    )
    batchsandbox_template_file: Optional[str] = Field(
        default=None,
        description="Path to BatchSandbox CR YAML template file. Used when workload_provider is 'batchsandbox'.",
    )
    sandbox_create_timeout_seconds: int = Field(
        default=60,
        ge=1,
        description="Timeout in seconds to wait for a sandbox to become ready (IP assigned) after creation.",
    )
    sandbox_create_poll_interval_seconds: float = Field(
        default=1.0,
        gt=0,
        description="Polling interval in seconds when waiting for a sandbox to become ready after creation.",
    )
    snapshot_create_timeout_seconds: int = Field(
        default=15 * 60,
        ge=1,
        description=(
            "Timeout in seconds to wait for a Kubernetes public snapshot to become ready. "
            "Set this greater than the controller snapshot commit-job-timeout."
        ),
    )
    execd_init_resources: Optional["ExecdInitResources"] = Field(
        default=None,
        description=(
            "Resource requests/limits for the execd init container. "
            "If unset, no resource constraints are applied."
        ),
    )
    image_pull_policy: Optional[str] = Field(
        default="IfNotPresent",
        description=(
            "Image pull policy for sandbox containers. "
            "Values: Always, IfNotPresent, Never. "
            "Can be overridden per-sandbox via image.pull_policy in create request."
        ),
    )


class ExecdInitResources(BaseModel):
    """Resource requests and limits for the execd init container."""

    limits: Optional[Dict[str, str]] = Field(
        default=None,
        description='Resource limits, e.g. {cpu = "100m", memory = "128Mi"}.',
    )
    requests: Optional[Dict[str, str]] = Field(
        default=None,
        description='Resource requests, e.g. {cpu = "50m", memory = "64Mi"}.',
    )


class AgentSandboxRuntimeConfig(BaseModel):
    """Agent-sandbox runtime configuration."""

    template_file: Optional[str] = Field(
        default=None,
        description="Path to Sandbox CR YAML template file for agent-sandbox.",
    )
    shutdown_policy: Literal["Delete", "Retain"] = Field(
        default="Delete",
        description="Shutdown policy applied when a sandbox expires (Delete or Retain).",
    )
    ingress_enabled: bool = Field(
        default=True,
        description="Whether ingress routing to agent-sandbox pods is expected to be enabled.",
    )


class StorageConfig(BaseModel):
    """Volume and storage configuration for sandbox mounts."""

    allowed_host_paths: list[str] = Field(
        default_factory=list,
        description=(
            "Allowlist of host path prefixes permitted for host bind mounts. "
            "If empty, host bind mounts are rejected. "
            "Each entry must be an absolute path (e.g., '/data/opensandbox')."
        ),
    )
    volume_default_size: str = Field(
        default="1Gi",
        description=(
            "Default storage size for auto-created PVCs when the caller does "
            "not specify a size in the PVC provisioning hints."
        ),
    )
    ossfs_mount_root: str = Field(
        default="/mnt/ossfs",
        description=(
            "Host-side root directory where OSSFS mounts are resolved. "
            "Resolved OSSFS host paths are built as "
            "'ossfs_mount_root/<bucket>/<volume.subPath?>'."
        ),
    )

DEFAULT_EGRESS_DISABLE_IPV6 = True

class EgressConfig(BaseModel):
    """Egress sidecar configuration."""

    image: Optional[str] = Field(
        default=None,
        description="Container image for the egress sidecar (used when network policy is requested).",
        min_length=1,
    )
    mode: Literal[
        EGRESS_MODE_DNS,
        EGRESS_MODE_DNS_NFT,
    ] = Field(
        default=EGRESS_MODE_DNS,
        description="Egress enforcement passed to the sidecar as OPENSANDBOX_EGRESS_MODE (dns or dns+nft).",
    )
    disable_ipv6: bool = Field(
        default=DEFAULT_EGRESS_DISABLE_IPV6,
        description=(
            "Default true: egress IPv6 support is incomplete, especially on Kubernetes runtime. "
            "Set false only if you intentionally leave IPv6 enabled in the sandbox netns "
            "(e.g. IPv4-only CNI or experimenting with IPv6 egress despite gaps)."
        ),
    )


class RuntimeConfig(BaseModel):
    """Runtime selection (docker, kubernetes, etc.)."""

    type: Literal["docker", "kubernetes"] = Field(
        ...,
        description="Active sandbox runtime implementation.",
    )
    execd_image: str = Field(
        ...,
        description="Container image that contains the execd binary for sandbox initialization.",
        min_length=1,
    )


class SecureRuntimeConfig(BaseModel):
    """Secure container runtime configuration (gVisor, Kata, Firecracker)."""

    type: Literal["", "gvisor", "kata", "firecracker"] = Field(
        default="",
        description=(
            "Secure runtime type. Empty means no secure runtime. "
            "gVisor uses runsc OCI runtime. "
            "Kata uses kata-runtime (OCI) or kata-qemu (RuntimeClass). "
            "Firecracker uses kata-fc (RuntimeClass, Kubernetes only)."
        ),
    )
    docker_runtime: Optional[str] = Field(
        default=None,
        description=(
            "OCI runtime name for Docker (e.g., 'runsc' for gVisor, 'kata-runtime' for Kata). "
            "When specified, the Docker daemon will use this runtime instead of runc."
        ),
    )
    k8s_runtime_class: Optional[str] = Field(
        default=None,
        description=(
            "Kubernetes RuntimeClass name for secure containers. "
            "Common values: 'gvisor', 'kata-qemu', 'kata-fc'. "
            "When specified, pods will have runtimeClassName set to this value."
        ),
    )

    @model_validator(mode="after")
    def validate_secure_runtime(self) -> "SecureRuntimeConfig":
        if self.type == "":
            # No secure runtime configured
            if self.docker_runtime is not None or self.k8s_runtime_class is not None:
                raise ValueError(
                    "docker_runtime and k8s_runtime_class must be omitted when secure_runtime.type is empty."
                )
            return self

        if self.type == "firecracker":
            # Firecracker is Kubernetes-only
            if self.k8s_runtime_class is None:
                raise ValueError(
                    "secure_runtime.k8s_runtime_class is required when secure_runtime.type is 'firecracker'."
                )
            # Optional: also allow docker_runtime for consistency, but Firecracker won't use it

        # For gVisor and Kata, at least one runtime must be specified
        if self.type in ("gvisor", "kata"):
            if self.docker_runtime is None and self.k8s_runtime_class is None:
                raise ValueError(
                    f"At least one of secure_runtime.docker_runtime or secure_runtime.k8s_runtime_class "
                    f"must be specified when secure_runtime.type is '{self.type}'."
                )

        return self


class DockerConfig(BaseModel):
    """Docker runtime specific settings."""

    network_mode: str = Field(
        default="host",
        description="Docker network mode for sandbox containers (host, bridge, or a custom user-defined network name).",
    )
    api_timeout: Optional[int] = Field(
        default=None,
        ge=1,
        description="Docker API timeout in seconds. If unset, default is 180.",
    )
    host_ip: Optional[str] = Field(
        default=None,
        description=(
            "Docker host IP or hostname for bridge-mode endpoint URLs when the server runs in a container."
        ),
    )
    drop_capabilities: list[str] = Field(
        default_factory=lambda: [
            "AUDIT_WRITE",
            "MKNOD",
            "NET_ADMIN",
            "NET_RAW",
            "SYS_ADMIN",
            "SYS_MODULE",
            "SYS_PTRACE",
            "SYS_TIME",
            "SYS_TTY_CONFIG",
        ],
        description=(
            "Linux capabilities to drop from sandbox containers. Defaults to a conservative set to reduce host impact."
        ),
    )
    apparmor_profile: Optional[str] = Field(
        default=None,
        description=(
            "Optional AppArmor profile name applied to sandbox containers. Leave unset to let Docker choose the default."
        ),
    )
    no_new_privileges: bool = Field(
        default=True,
        description="Enable the kernel no_new_privileges flag to block privilege escalation inside the container.",
    )
    seccomp_profile: Optional[str] = Field(
        default=None,
        description=(
            "Optional seccomp profile name or path applied to sandbox containers. Leave unset to use Docker's default profile."
        ),
    )
    pids_limit: Optional[int] = Field(
        default=4096,
        ge=1,
        description="Maximum number of processes allowed per sandbox container. Set to null to disable the limit.",
    )


class StoreConfig(BaseModel):
    """Persistence backend for server-managed server resources."""

    type: Literal["sqlite"] = Field(
        default="sqlite",
        description="Server persistence backend type. SQLite is the default local persistent backend.",
    )
    path: str = Field(
        default=str(Path.home() / ".opensandbox" / "opensandbox.db"),
        description="Filesystem path to the SQLite database used for server metadata persistence.",
        min_length=1,
    )


class AppConfig(BaseModel):
    """Root application configuration model."""

    server: ServerConfig = Field(default_factory=ServerConfig)
    log: LogConfig = Field(
        default_factory=LogConfig,
        description="Logging configuration (level, file output, rotation).",
    )
    renew_intent: RenewIntentConfig = Field(
        default_factory=RenewIntentConfig,
        description="Auto-renew sandbox expiration when reverse-proxy access is observed.",
    )
    runtime: RuntimeConfig = Field(..., description="Sandbox runtime configuration.")
    kubernetes: Optional[KubernetesRuntimeConfig] = None
    agent_sandbox: Optional["AgentSandboxRuntimeConfig"] = None
    ingress: Optional[IngressConfig] = None
    docker: DockerConfig = Field(default_factory=DockerConfig)
    storage: StorageConfig = Field(default_factory=StorageConfig)
    store: StoreConfig = Field(
        default_factory=StoreConfig,
        description="Persistence backend configuration for server-managed resources.",
    )
    egress: Optional[EgressConfig] = None
    secure_runtime: Optional[SecureRuntimeConfig] = Field(
        default=None,
        description="Secure container runtime configuration (gVisor, Kata, Firecracker).",
    )
    @model_validator(mode="after")
    def validate_runtime_blocks(self) -> "AppConfig":
        if self.runtime.type == "docker":
            if self.kubernetes is not None:
                raise ValueError("Kubernetes block must be omitted when runtime.type = 'docker'.")
            if self.agent_sandbox is not None:
                raise ValueError("agent_sandbox block must be omitted when runtime.type = 'docker'.")
            if self.ingress is not None and self.ingress.mode != INGRESS_MODE_DIRECT:
                raise ValueError("ingress.mode must be 'direct' when runtime.type = 'docker'.")
            if self.secure_runtime is not None and self.secure_runtime.type == "firecracker":
                raise ValueError( "secure_runtime.type 'firecracker' is only compatible with runtime.type='kubernetes'.")
        elif self.runtime.type == "kubernetes":
            if self.kubernetes is None:
                self.kubernetes = KubernetesRuntimeConfig()
            provider_type = (self.kubernetes.workload_provider or "").lower()
            if provider_type == "agent-sandbox":
                if self.agent_sandbox is None:
                    self.agent_sandbox = AgentSandboxRuntimeConfig()
            elif self.agent_sandbox is not None:
                raise ValueError(
                    "agent_sandbox block requires kubernetes.workload_provider = 'agent-sandbox'."
                )
        else:
            raise ValueError(f"Unsupported runtime type '{self.runtime.type}'.")
        return self


_config: AppConfig | None = None
_config_path: Path | None = None


def _resolve_config_path(path: str | Path | None = None) -> Path:
    """Resolve configuration file path from explicit value, env var, or default."""
    if path:
        return Path(path).expanduser()
    env_path = os.environ.get(CONFIG_ENV_VAR)
    if env_path:
        return Path(env_path).expanduser()
    return DEFAULT_CONFIG_PATH


def _load_toml_data(path: Path) -> dict[str, Any]:
    """Load TOML content from file, returning empty dict if file is missing."""
    if not path.exists():
        logger.info("Config file %s not found. Using default configuration.", path)
        return {}

    try:
        with path.open("rb") as fh:
            data = tomllib.load(fh)
            logger.info("Loaded configuration from %s", path)
            return data
    except Exception as exc:  # noqa: BLE001
        logger.error("Failed to read config file %s: %s", path, exc)
        raise


def _apply_env_overrides(config: AppConfig) -> None:
    """Apply environment variable overrides to parsed configuration."""
    if API_KEY_ENV_VAR in os.environ:
        config.server.api_key = os.environ[API_KEY_ENV_VAR]


def load_config(path: str | Path | None = None) -> AppConfig:
    """
    Load configuration from TOML file and store it globally.

    Args:
        path: Optional explicit config path. Falls back to SANDBOX_CONFIG_PATH env,
              then ~/.sandbox.toml when not provided.

    Returns:
        AppConfig: Parsed application configuration.

    Raises:
        ValidationError: If the TOML contents do not match AppConfig schema.
        Exception: For any IO or parsing errors.
    """
    global _config, _config_path

    resolved_path = _resolve_config_path(path)
    raw_data = _load_toml_data(resolved_path)

    try:
        _config = AppConfig(**raw_data)
    except ValidationError as exc:
        logger.error("Invalid configuration in %s: %s", resolved_path, exc)
        raise

    _apply_env_overrides(_config)
    _config_path = resolved_path
    return _config


def get_config() -> AppConfig:
    """
    Retrieve the currently loaded configuration, loading defaults if necessary.

    Returns:
        AppConfig: Currently active configuration.
    """
    global _config
    if _config is None:
        _config = load_config()
    return _config


def get_config_path() -> Path:
    """Return the resolved configuration path."""
    global _config_path
    if _config_path is None:
        _config_path = _resolve_config_path()
    return _config_path


__all__ = [
    "AppConfig",
    "RenewIntentConfig",
    "RenewIntentRedisConfig",
    "ServerConfig",
    "LogConfig",
    "RuntimeConfig",
    "IngressConfig",
    "GatewayConfig",
    "GatewayRouteModeConfig",
    "SecureAccessConfig",
    "SecureAccessKey",
    "INGRESS_MODE_DIRECT",
    "INGRESS_MODE_GATEWAY",
    "DockerConfig",
    "StorageConfig",
    "StoreConfig",
    "KubernetesRuntimeConfig",
    "EgressConfig",
    "EGRESS_MODE_DNS",
    "EGRESS_MODE_DNS_NFT",
    "SecureRuntimeConfig",
    "DEFAULT_CONFIG_PATH",
    "CONFIG_ENV_VAR",
    "get_config",
    "get_config_path",
    "load_config",
]
