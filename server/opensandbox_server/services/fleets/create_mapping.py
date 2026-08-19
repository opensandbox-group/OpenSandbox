# pyright: reportAttributeAccessIssue=false
# protobuf-generated modules expose dynamic attributes.

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

"""Map OpenSandbox CreateSandboxRequest into fast-sandbox FastPath v2 CreateRequest.

The fleets backend accepts a strict subset of the public create contract.
Unsupported fields are rejected with a clear error instead of being silently
ignored (see OSEP-0007 "Simplified Create").
"""

from __future__ import annotations

import decimal
import re
from datetime import datetime, timezone
from typing import Optional

from opensandbox_server.api.schema import CreateSandboxRequest
from opensandbox_server.services.fleets.generated import (
    fastpath_pb2 as pb2,
)

#: fleets-reserved FastPath metadata key persisting the renew-on-access
#: extension value. Stripped from public metadata and list filters.
#: fast-sandbox persists metadata as labels under `metadata.sandbox.fast.io/`,
#: so the key must be a DNS1123 label (lowercase alphanumeric + hyphens).
RENEW_EXTEND_SECONDS_METADATA_KEY = "renew-extend-seconds"

#: Public extensions keys accepted by the fleets backend.
SUPPORTED_EXTENSION_KEYS = frozenset(
    {"poolRef", "access.renew.extend.seconds"}
)

#: FastPath CreateRequest has no nullable timeout; fleets requires an explicit one.
ERROR_TIMEOUT_REQUIRED = (
    "timeout is required on fleets: fast-sandbox persists an absolute "
    "expires_at in the first Create write and has no non-expiring sandboxes."
)


class UnsupportedFieldError(ValueError):
    """A CreateSandboxRequest field cannot be honored by the fleets backend."""

    def __init__(self, field: str, reason: str):
        super().__init__(f"{field}: {reason}")
        self.field = field
        self.reason = reason


def map_create_request(
    request: CreateSandboxRequest,
    sandbox_id: str,
    namespace: str,
    *,
    default_pool_ref: str = "default-pool",
    now: Optional[datetime] = None,
    expires_at_unix_seconds: Optional[int] = None,
    pool_resources: Optional[dict] = None,
) -> pb2.CreateRequest:
    """Map the accepted CreateSandboxRequest subset to a FastPath v2 CreateRequest.

    Raises UnsupportedFieldError for any field the shared-Fastlet model cannot
    honor. The caller supplies the OpenSandbox sandbox_id, which becomes the
    idempotency key (request_id) and the Sandbox CRD name.

    Idempotency: FastPath treats expiry as part of the persisted initial
    intent, so a transport retry of the same sandbox_id must reuse the first
    absolute expiry. Callers can pass the previously normalized
    ``expires_at_unix_seconds``; when omitted it is derived from ``now``.

    Pool compatibility: ``pool_resources`` is the selected SandboxPool profile
    (e.g. {"cpu": "500m", "memory": "512Mi", "pids": "256"}). When provided,
    request ``resource_limits`` must match the pool for every key the pool
    defines and must not declare keys the pool does not define; FastPath has
    no per-sandbox resource field, so incompatible limits are rejected rather
    than silently ignored.
    """
    _reject_unsupported_fields(request)

    if pool_resources is not None:
        _validate_resource_limits(request, pool_resources)

    image = request.image
    if image is None or not image.uri.strip():
        # A fast-sandbox SandboxPool defines Infra Components and resources,
        # not the workload image, so fleets rejects image-less requests even
        # when extensions.poolRef is set.
        raise UnsupportedFieldError("image", "a non-empty image.uri is required on fleets")

    if image.auth is not None:
        raise UnsupportedFieldError(
            "image.auth", "private-registry credentials are not carried to fast-sandbox"
        )

    if request.timeout is None:
        raise UnsupportedFieldError("timeout", ERROR_TIMEOUT_REQUIRED)

    if request.entrypoint is None:
        raise UnsupportedFieldError("entrypoint", "entrypoint is required when image is provided")

    if expires_at_unix_seconds is not None:
        expires_at = expires_at_unix_seconds
    else:
        now = now or datetime.now(timezone.utc)
        expires_at = int(now.timestamp()) + request.timeout

    create = pb2.CreateRequest(
        request_id=sandbox_id,
        namespace=namespace,
        image=image.uri,
        command=list(request.entrypoint),
        expires_at_unix_seconds=expires_at,
    )

    if request.env:
        for key, value in request.env.items():
            if value is None:
                raise UnsupportedFieldError(
                    "env",
                    f"null value for environment variable {key!r} is not supported "
                    "(FastPath v2 uses map<string,string>)",
                )
            create.envs[key] = value

    if request.metadata:
        create.metadata.update(request.metadata)

    extensions = request.extensions or {}
    # Normalize before forwarding: a whitespace-only poolRef must not select
    # an invalid pool, and a padded name must not reach FastPath as-is.
    pool_ref = (extensions.get("poolRef") or "").strip()
    create.pool_ref = pool_ref or default_pool_ref

    renew_value = extensions.get("access.renew.extend.seconds")
    if renew_value is not None:
        create.metadata[RENEW_EXTEND_SECONDS_METADATA_KEY] = renew_value

    # fast-sandbox persists metadata as labels (metadata.sandbox.fast.io/<key>)
    # and validates every entry; reject incompatible keys/values here so users
    # get a clear error instead of a confusing gRPC rejection.
    _validate_metadata(create.metadata)

    return create


def _validate_resource_limits(
    request: CreateSandboxRequest, pool_resources: dict
) -> None:
    if request.resource_limits is None:
        return
    limits = request.resource_limits.root
    for key, value in limits.items():
        if key not in pool_resources:
            raise UnsupportedFieldError(
                "resourceLimits",
                f"{key!r} is not defined by the selected SandboxPool; "
                "resources are fixed by SandboxPool.spec.sandboxResources",
            )
        if not _quantities_equal(pool_resources[key], value):
            raise UnsupportedFieldError(
                "resourceLimits",
                f"{key!r}={value!r} does not match the SandboxPool profile "
                f"{key!r}={pool_resources[key]!r}; resources are fixed per pool",
            )


#: Kubernetes quantity decimal-power suffixes (m is handled separately).
_DECIMAL_QUANTITY_SUFFIXES = {"k": 3, "M": 6, "G": 9, "T": 12, "P": 15, "E": 18}
#: Kubernetes quantity binary-power suffixes.
_BINARY_QUANTITY_SUFFIXES = {"Ki": 10, "Mi": 20, "Gi": 30, "Ti": 40, "Pi": 50, "Ei": 60}

_DNS_LABEL_PATTERN = r"^[a-z0-9]([-a-z0-9]*[a-z0-9])?$"
_LABEL_VALUE_PATTERN = r"^[A-Za-z0-9]([-_.A-Za-z0-9]*[A-Za-z0-9])?$"
_MAX_LABEL_LENGTH = 63


def _validate_metadata(metadata: dict) -> None:
    """Reject metadata entries fast-sandbox cannot persist as labels."""
    for key, value in metadata.items():
        if (
            len(key) > _MAX_LABEL_LENGTH
            or not re.fullmatch(_DNS_LABEL_PATTERN, key)
        ):
            raise UnsupportedFieldError(
                "metadata",
                f"metadata key {key!r} must be a DNS label (lowercase alphanumeric "
                "and hyphens, max 63 chars): fast-sandbox persists metadata as labels",
            )
        if (
            len(value) > _MAX_LABEL_LENGTH
            or not re.fullmatch(_LABEL_VALUE_PATTERN, value)
        ):
            raise UnsupportedFieldError(
                "metadata",
                f"metadata value for {key!r} must be a Kubernetes label value "
                "(alphanumeric, '-', '_', '.', max 63 chars)",
            )


def _quantities_equal(a: str, b: str) -> bool:
    """Compare Kubernetes resource quantities canonically ("0.5" == "500m", "1Gi" == "1024Mi")."""
    try:
        return _canonical_quantity(a) == _canonical_quantity(b)
    except Exception:
        # Fall back to the raw comparison for unparseable values so the
        # rejection message stays accurate.
        return a == b


def _canonical_quantity(value: str) -> decimal.Decimal:
    value = value.strip()
    if value.endswith("m"):
        return decimal.Decimal(value[:-1]) / 1000
    for suffix, exponent in _BINARY_QUANTITY_SUFFIXES.items():
        if value.endswith(suffix):
            return decimal.Decimal(value[: -len(suffix)]) * (decimal.Decimal(2) ** exponent)
    for suffix, exponent in _DECIMAL_QUANTITY_SUFFIXES.items():
        if value.endswith(suffix):
            return decimal.Decimal(value[: -len(suffix)]) * (decimal.Decimal(10) ** exponent)
    return decimal.Decimal(value)


def _reject_unsupported_fields(request: CreateSandboxRequest) -> None:
    """Reject pod-identity-dependent fields that have no shared-Fastlet meaning."""
    if request.snapshot_id:
        raise UnsupportedFieldError("snapshotId", "snapshots are not supported on fleets")
    if request.platform is not None:
        raise UnsupportedFieldError(
            "platform", "scheduling is per Fastlet pool, not per sandbox"
        )
    if request.resource_requests is not None:
        raise UnsupportedFieldError(
            "resourceRequests",
            "resources are fixed by SandboxPool.spec.sandboxResources",
        )
    if request.credential_proxy is not None and request.credential_proxy.enabled:
        raise UnsupportedFieldError(
            "credentialProxy",
            "credential proxy rides the per-pod egress sidecar, which does not "
            "exist in the shared-Fastlet model",
        )
    if request.volumes:
        raise UnsupportedFieldError(
            "volumes", "Fastlet child containers cannot receive dynamic mounts"
        )
    if request.network_policy is not None:
        raise UnsupportedFieldError(
            "networkPolicy",
            "per-sandbox egress enforcement is deferred to phase 1b; not supported in phase 1a",
        )
    if request.secure_access:
        raise UnsupportedFieldError(
            "secureAccess",
            "secure access on fleets is deferred to phase 1b; not supported in phase 1a",
        )
    for key in (request.extensions or {}):
        if key not in SUPPORTED_EXTENSION_KEYS:
            raise UnsupportedFieldError(
                f"extensions[{key!r}]",
                "extension keys are rejected unless explicitly supported by fleets",
            )
