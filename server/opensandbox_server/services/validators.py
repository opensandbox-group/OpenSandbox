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
Shared validation helpers for container-based sandbox services.

These helpers centralize request validation so all container runtimes
enforce the same preconditions before performing runtime-specific work.
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import TYPE_CHECKING, Dict, List, Optional, Sequence

from fastapi import HTTPException, status
import re

from opensandbox_server.services.constants import RESERVED_LABEL_PREFIX, SandboxErrorCodes

if TYPE_CHECKING:
    from opensandbox_server.api.schema import NetworkPolicy, OSSFS, PlatformSpec, Volume
    from opensandbox_server.config import EgressConfig


def ensure_entrypoint(entrypoint: Sequence[str]) -> None:
    """
    Ensure a sandbox entrypoint is provided.

    Raises:
        HTTPException: When entrypoint is empty.
    """
    if not entrypoint:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_ENTRYPOINT,
                "message": "Entrypoint must contain at least one command.",
            },
        )


DNS_LABEL_PATTERN = r"[a-z0-9]([-a-z0-9]*[a-z0-9])?"
DNS_SUBDOMAIN_RE = re.compile(rf"^(?:{DNS_LABEL_PATTERN}\.)*{DNS_LABEL_PATTERN}$")
LABEL_NAME_RE = re.compile(r"^[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$")
LABEL_VALUE_RE = re.compile(r"^([A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?)?$")
HOST_PATH_RE = re.compile(r"^(/|[A-Za-z]:[\\/])")


def _normalize_prefix_path(path: str) -> str:
    normalized = path.replace("\\", "/")
    # Windows drive letters are case-insensitive; canonicalize for comparisons.
    if re.match(r"^[A-Za-z]:", normalized):
        normalized = normalized[0].lower() + normalized[1:]
    if len(normalized) > 1 and normalized.endswith("/"):
        return normalized[:-1]
    return normalized


def _is_valid_label_key(key: str) -> bool:
    if "/" in key:
        prefix, name = key.split("/", 1)
        if not prefix or not name:
            return False
        # Kubernetes requires the prefix to be a DNS subdomain <= 253 chars.
        # The name portion is validated separately below (max 63 chars).
        # Note: the total key length (prefix + "/" + name) may exceed 253 chars
        # when the prefix uses its full 253-character allowance; this is valid.
        if len(prefix) > 253:
            return False
        if not DNS_SUBDOMAIN_RE.match(prefix):
            return False
    else:
        name = key
    if len(name) > 63 or not LABEL_NAME_RE.match(name):
        return False
    return True


def _is_valid_label_value(value: str) -> bool:
    if len(value) > 63:
        return False
    return bool(LABEL_VALUE_RE.match(value))


def ensure_metadata_labels(metadata: Optional[Dict[str, str]]) -> None:
    """
    Validate metadata keys/values against Kubernetes label rules.

    Raises:
        HTTPException: When a key/value is invalid.
    """
    if not metadata:
        return
    for key, value in metadata.items():
        if key.startswith(RESERVED_LABEL_PREFIX):
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_METADATA_LABEL,
                    "message": (
                        f"Metadata key '{key}' uses the reserved prefix '{RESERVED_LABEL_PREFIX}'. "
                        "Keys under this prefix are managed by the system and cannot be set via metadata."
                    ),
                },
            )
        if not _is_valid_label_key(key):
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_METADATA_LABEL,
                    "message": f"Metadata key '{key}' is invalid: must be either a name or a DNS-subdomain prefix and name separated by /, where the name is up to 63 characters and matches [A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?, and the optional prefix is a valid DNS subdomain up to 253 characters.",
                },
            )
        if not _is_valid_label_value(value):
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_METADATA_LABEL,
                    "message": f"Metadata value '{value}' is invalid: must be 63 characters or less, start/end with an alphanumeric character, and contain only alphanumeric, '-', '_', or '.' characters.",
                },
            )


def ensure_future_expiration(expires_at: datetime) -> datetime:
    """
    Validate and normalize expiration timestamps to UTC.

    Args:
        expires_at: Requested expiration time (timezone aware or naive).

    Returns:
        datetime: Normalized UTC expiration timestamp.

    Raises:
        HTTPException: If the timestamp is not in the future.
    """
    if expires_at.tzinfo is None:
        normalized = expires_at.replace(tzinfo=timezone.utc)
    else:
        normalized = expires_at.astimezone(timezone.utc)

    if normalized <= datetime.now(timezone.utc):
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_EXPIRATION,
                "message": "New expiration time must be in the future.",
            },
        )

    return normalized


def ensure_valid_port(port: int) -> None:
    """
    Validate that a port falls within the 1-65535 range.

    Raises:
        HTTPException: When the port is out of range.
    """
    if port < 1 or port > 65535:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PORT,
                "message": "Port must be between 1 and 65535.",
            },
        )


def ensure_timeout_within_limit(timeout_seconds: Optional[int], max_timeout_seconds: Optional[int]) -> None:
    """
    Validate that a requested sandbox TTL does not exceed the configured limit.

    Args:
        timeout_seconds: Requested sandbox TTL in seconds, or None for manual cleanup.
        max_timeout_seconds: Configured maximum TTL in seconds, or None to disable the limit.

    Raises:
        HTTPException: When the timeout exceeds the configured maximum.
    """
    if timeout_seconds is None:
        return

    calculate_expiration_or_raise(datetime.now(timezone.utc), timeout_seconds)

    if max_timeout_seconds is None:
        return

    if timeout_seconds > max_timeout_seconds:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    f"Sandbox timeout {timeout_seconds}s exceeds configured maximum "
                    f"of {max_timeout_seconds}s."
                ),
            },
        )


def calculate_expiration_or_raise(created_at: datetime, timeout_seconds: int) -> datetime:
    """
    Compute an expiration timestamp and convert datetime overflow into a 400 error.

    Raises:
        HTTPException: When the timeout value is too large to represent safely.
    """
    try:
        return created_at + timedelta(seconds=timeout_seconds)
    except (OverflowError, ValueError) as exc:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    f"Sandbox timeout {timeout_seconds}s is too large to represent safely."
                ),
            },
        ) from exc


def ensure_platform_valid(platform: Optional["PlatformSpec"]) -> None:
    """
    Validate platform os/arch values for v1 platform contract.

    Supported values in this iteration:
    - os: linux, windows
    - arch:
      - linux: amd64, arm64
      - windows: amd64, arm64
    """
    if platform is None:
        return

    supported_os = {"linux", "windows"}
    supported_arch = {"amd64", "arm64"}
    normalized_os = platform.os.strip().lower()
    normalized_arch = platform.arch.strip().lower()

    if normalized_os not in supported_os:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    f"Unsupported platform.os '{platform.os}'. "
                    f"Supported values: {sorted(supported_os)}."
                ),
            },
        )
    if normalized_arch not in supported_arch:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": (
                    f"Unsupported platform.arch '{platform.arch}'. "
                    f"Supported values: {sorted(supported_arch)}."
                ),
            },
        )
    platform.os = normalized_os
    platform.arch = normalized_arch


# Volume name must be a valid DNS label
VOLUME_NAME_RE = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")
# Kubernetes resource name pattern
K8S_RESOURCE_NAME_RE = re.compile(r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$")


def ensure_valid_volume_name(name: str) -> None:
    """
    Validate that a volume name is a valid DNS label.

    Args:
        name: Volume name to validate.

    Raises:
        HTTPException: When the name is invalid.
    """
    if not name:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_VOLUME_NAME,
                "message": "Volume name cannot be empty.",
            },
        )
    if len(name) > 63:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_VOLUME_NAME,
                "message": f"Volume name '{name}' exceeds maximum length of 63 characters.",
            },
        )
    if not VOLUME_NAME_RE.match(name):
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_VOLUME_NAME,
                "message": f"Volume name '{name}' is not a valid DNS label. Must be lowercase alphanumeric with optional hyphens.",
            },
        )


def ensure_valid_mount_path(mount_path: str) -> None:
    """
    Validate that a mount path is an absolute path.

    Args:
        mount_path: Mount path to validate.

    Raises:
        HTTPException: When the path is not absolute.
    """
    if not mount_path:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_MOUNT_PATH,
                "message": "Mount path cannot be empty.",
            },
        )
    if not mount_path.startswith("/"):
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_MOUNT_PATH,
                "message": f"Mount path '{mount_path}' must be an absolute path starting with '/'.",
            },
        )


def ensure_valid_sub_path(sub_path: Optional[str]) -> None:
    """
    Validate that a subPath does not contain path traversal or is absolute.

    Args:
        sub_path: SubPath to validate (optional).

    Raises:
        HTTPException: When the subPath is invalid.
    """
    if sub_path is None:
        return

    if not sub_path:
        # Empty string is valid (no subpath)
        return

    # Check for absolute path
    if sub_path.startswith("/"):
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_SUB_PATH,
                "message": f"SubPath '{sub_path}' must be a relative path, not absolute.",
            },
        )

    # Check for path traversal
    # Normalize and check each component
    parts = sub_path.split("/")
    for part in parts:
        if part == "..":
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_SUB_PATH,
                    "message": f"SubPath '{sub_path}' contains path traversal '..' which is not allowed.",
                },
            )


def ensure_valid_host_path(
    path: str,
    allowed_prefixes: Optional[List[str]] = None,
) -> None:
    """
    Validate that a host path is absolute and optionally within allowed prefixes.

    Args:
        path: Host path to validate.
        allowed_prefixes: Optional list of allowed path prefixes.

    Raises:
        HTTPException: When the path is invalid or not allowed.
    """
    if not path:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_HOST_PATH,
                "message": "Host path cannot be empty.",
            },
        )

    if not HOST_PATH_RE.match(path):
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_HOST_PATH,
                "message": (
                    f"Host path '{path}' must be an absolute path starting with '/' "
                    "or a Windows drive letter (e.g. 'C:\\' or 'D:/')."
                ),
            },
        )

    # Normalize separators to forward slashes for consistent security checks.
    # Keep checks cross-platform by parsing drive prefixes without relying on
    # os.path.splitdrive behavior of the host OS.
    _path_fwd = path.replace("\\", "/")
    _windows_drive_match = re.match(r"^[A-Za-z]:/", _path_fwd)
    _tail_fwd = _path_fwd[2:] if _windows_drive_match else _path_fwd

    # Reject path traversal components
    if "/.." in _tail_fwd or _tail_fwd == "/..":
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_HOST_PATH,
                "message": f"Host path '{path}' contains path traversal component '..'.",
            },
        )

    # Reject non-normalized paths (double slashes, trailing slashes except root)
    if "//" in _tail_fwd or (len(_tail_fwd) > 1 and _tail_fwd.endswith("/")):
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_HOST_PATH,
                "message": f"Host path '{path}' is not normalized. Remove redundant slashes.",
            },
        )

    # Check against allowed prefixes if provided
    if allowed_prefixes is not None:
        # Normalize separators for cross-platform prefix checks so Windows-style
        # paths can be validated consistently even when server runs on Unix.
        norm_path = _normalize_prefix_path(path)
        is_allowed = any(
            norm_path == _normalize_prefix_path(prefix)
            or norm_path.startswith(_normalize_prefix_path(prefix) + "/")
            for prefix in allowed_prefixes
        )
        if not is_allowed:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.HOST_PATH_NOT_ALLOWED,
                    "message": f"Host path '{path}' is not under any allowed prefix. Allowed prefixes: {allowed_prefixes}",
                },
            )


def ensure_valid_pvc_name(claim_name: str) -> None:
    """
    Validate that a PVC claim name is a valid Kubernetes resource name.

    Args:
        claim_name: PVC claim name to validate.

    Raises:
        HTTPException: When the claim name is invalid.
    """
    if not claim_name:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PVC_NAME,
                "message": "PVC claim name cannot be empty.",
            },
        )
    if len(claim_name) > 253:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PVC_NAME,
                "message": f"PVC claim name '{claim_name}' exceeds maximum length of 253 characters.",
            },
        )
    if not K8S_RESOURCE_NAME_RE.match(claim_name):
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PVC_NAME,
                "message": f"PVC claim name '{claim_name}' is not a valid Kubernetes resource name.",
            },
        )


def ensure_valid_ossfs_volume(ossfs: "OSSFS") -> None:
    """
    Validate OSSFS backend fields.

    Args:
        ossfs: OSSFS backend model.

    Raises:
        HTTPException: When any OSSFS field is invalid.
    """
    if not isinstance(ossfs.bucket, str) or not ossfs.bucket.strip():
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_OSSFS_BUCKET,
                "message": "OSSFS bucket cannot be empty.",
            },
        )

    if not ossfs.endpoint.strip():
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_OSSFS_ENDPOINT,
                "message": "OSSFS endpoint cannot be empty.",
            },
        )

    if ossfs.options is not None:
        for opt in ossfs.options:
            if not isinstance(opt, str) or not opt.strip():
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_OSSFS_OPTION,
                        "message": "OSSFS options must be non-empty strings.",
                    },
                )
            normalized = opt.strip()
            if normalized.startswith("-"):
                raise HTTPException(
                    status_code=status.HTTP_400_BAD_REQUEST,
                    detail={
                        "code": SandboxErrorCodes.INVALID_OSSFS_OPTION,
                        "message": (
                            "OSSFS options must be raw option payloads without '-' prefix "
                            "(e.g. 'allow_other', 'uid=1000')."
                        ),
                    },
                )

    if not ossfs.access_key_id or not ossfs.access_key_secret:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_OSSFS_CREDENTIALS,
                "message": (
                    "OSSFS inline credentials are required: "
                    "accessKeyId and accessKeySecret must be provided."
                ),
            },
        )


def ensure_egress_configured(
    network_policy: Optional["NetworkPolicy"],
    egress_config: Optional["EgressConfig"],
) -> None:
    """
    Validate that egress.image is configured when network policy is provided.
    
    This is a common validation shared by Docker and Kubernetes runtimes.
    
    Args:
        network_policy: Optional network policy from the request.
        egress_config: Optional egress configuration from app config.
    
    Raises:
        HTTPException: When network_policy is provided but egress.image is not configured.
    """
    if not network_policy:
        return
    
    egress_image = egress_config.image if egress_config else None
    if not egress_image:
        raise HTTPException(
            status_code=status.HTTP_400_BAD_REQUEST,
            detail={
                "code": SandboxErrorCodes.INVALID_PARAMETER,
                "message": "egress.image must be configured when networkPolicy is provided.",
            },
        )


def ensure_volumes_valid(
    volumes: Optional[List["Volume"]],
    allowed_host_prefixes: Optional[List[str]] = None,
) -> None:
    """
    Validate a list of volume definitions.

    This function performs comprehensive validation:
    - Unique volume names
    - Exactly one backend per volume
    - Valid mount paths
    - Valid subPaths
    - Backend-specific validation (host path, pvc name, ossfs config)

    Args:
        volumes: List of volumes to validate (optional).
        allowed_host_prefixes: Optional list of allowed host path prefixes.

    Raises:
        HTTPException: When any validation fails.
    """
    if volumes is None or len(volumes) == 0:
        return

    # Check for duplicate volume names
    seen_names: set[str] = set()
    for volume in volumes:
        if volume.name in seen_names:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.DUPLICATE_VOLUME_NAME,
                    "message": f"Duplicate volume name '{volume.name}'. Each volume must have a unique name.",
                },
            )
        seen_names.add(volume.name)

        # Validate volume name
        ensure_valid_volume_name(volume.name)

        # Validate mount path
        ensure_valid_mount_path(volume.mount_path)

        # Validate subPath
        ensure_valid_sub_path(volume.sub_path)

        # Count specified backends
        backends_specified = sum([
            volume.host is not None,
            volume.pvc is not None,
            volume.ossfs is not None,
        ])

        if backends_specified == 0:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_VOLUME_BACKEND,
                    "message": (
                        f"Volume '{volume.name}' must specify exactly one backend "
                        "(host, pvc, ossfs), but none was provided."
                    ),
                },
            )

        if backends_specified > 1:
            raise HTTPException(
                status_code=status.HTTP_400_BAD_REQUEST,
                detail={
                    "code": SandboxErrorCodes.INVALID_VOLUME_BACKEND,
                    "message": (
                        f"Volume '{volume.name}' must specify exactly one backend "
                        "(host, pvc, ossfs), but multiple were provided."
                    ),
                },
            )

        # Backend-specific validation
        if volume.host is not None:
            ensure_valid_host_path(volume.host.path, allowed_host_prefixes)

        if volume.pvc is not None:
            ensure_valid_pvc_name(volume.pvc.claim_name)

        if volume.ossfs is not None:
            ensure_valid_ossfs_volume(volume.ossfs)


__all__ = [
    "ensure_entrypoint",
    "ensure_future_expiration",
    "ensure_valid_port",
    "ensure_platform_valid",
    "ensure_metadata_labels",
    "ensure_egress_configured",
    "ensure_valid_volume_name",
    "ensure_valid_mount_path",
    "ensure_valid_sub_path",
    "ensure_valid_host_path",
    "ensure_valid_pvc_name",
    "ensure_valid_ossfs_volume",
    "ensure_volumes_valid",
]
