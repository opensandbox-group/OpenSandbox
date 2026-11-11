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

from __future__ import annotations

from typing import TYPE_CHECKING, Dict, Mapping, MutableMapping, Optional

from opensandbox_server.extensions.keys import (
    ACCESS_RENEW_EXTEND_SECONDS_KEY,
    ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY,
    EXTENSIONS_ANNOTATION_PREFIX,
    ANNOTATION_METADATA_PREFIX,
)

if TYPE_CHECKING:
    from opensandbox_server.api.schema import SandboxLifecycle


def access_renew_extend_seconds_from_lifecycle(
    lifecycle: Optional["SandboxLifecycle"],
) -> Optional[int]:
    """Renew-on-access seconds from ``lifecycle.renewalPolicy``, or None."""
    if lifecycle is None:
        return None
    policy = lifecycle.renewal_policy
    if policy is None:
        return None
    return int(policy.extend_seconds)


def apply_access_renew_extend_seconds_to_mapping(
    mapping: MutableMapping[str, str],
    extensions: Optional[Dict[str, str]],
    *,
    lifecycle: Optional["SandboxLifecycle"] = None,
    metadata_key: str = ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY,
) -> None:
    """
    Persist the renew-on-access opt-in onto ``mapping`` (Kubernetes
    annotations or Docker labels): ``lifecycle.renewalPolicy`` preferred,
    deprecated extensions key as fallback (values already validated equal).

    TODO(2.0): drop the legacy ``extensions`` fallback with the request
    entry (#2289); the lifecycle branch and metadata key stay.
    """
    declared = access_renew_extend_seconds_from_lifecycle(lifecycle)
    if declared is not None:
        mapping[metadata_key] = str(declared)
        return
    if not extensions:
        return
    raw = extensions.get(ACCESS_RENEW_EXTEND_SECONDS_KEY)
    if raw is None:
        return
    s = str(raw).strip()
    if not s:
        return
    mapping[metadata_key] = s


def apply_extensions_to_mapping(
    annotations: MutableMapping[str, str],
    extensions: Optional[Dict[str, str]],
) -> None:
    """
    Propagate extension keys with ``opensandbox.extensions.`` prefix to Pod annotations.

    For each extension key starting with EXTENSIONS_ANNOTATION_PREFIX, the value is
    copied to annotations with ANNOTATION_METADATA_PREFIX prefix.

    Example:
        extensions = {"opensandbox.extensions.pool-ref": "my-pool"}
        → annotations["opensandbox.io/extensions.pool-ref"] = "my-pool"
    """
    if not extensions:
        return
    for key, value in extensions.items():
        if key.startswith(EXTENSIONS_ANNOTATION_PREFIX):
            # Extract the suffix after the prefix
            suffix = key[len(EXTENSIONS_ANNOTATION_PREFIX):]
            annotation_key = ANNOTATION_METADATA_PREFIX + suffix
            annotations[annotation_key] = value


def extract_extensions_from_mapping(
    annotations: Optional[Mapping[str, str]],
) -> Optional[Dict[str, str]]:
    """
    Restore extension keys previously propagated to Pod annotations.

    For each annotation key starting with ANNOTATION_METADATA_PREFIX, the value is
    copied to extensions with EXTENSIONS_ANNOTATION_PREFIX prefix. Empty values are
    treated as unset: Docker labels cannot be removed, so an extension inherited
    from a snapshot image is blanked instead.
    """
    if not annotations:
        return None

    extensions = {
        EXTENSIONS_ANNOTATION_PREFIX + key[len(ANNOTATION_METADATA_PREFIX):]: value
        for key, value in annotations.items()
        if key.startswith(ANNOTATION_METADATA_PREFIX) and value
    }
    return extensions or None
