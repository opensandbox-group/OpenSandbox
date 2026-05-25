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

from __future__ import annotations

from typing import Dict, MutableMapping, Optional

from opensandbox_server.extensions.keys import (
    ACCESS_RENEW_EXTEND_SECONDS_KEY,
    ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY,
    EXTENSIONS_ANNOTATION_PREFIX,
    ANNOTATION_METADATA_PREFIX,
)


def apply_access_renew_extend_seconds_to_mapping(
    mapping: MutableMapping[str, str],
    extensions: Optional[Dict[str, str]],
    *,
    metadata_key: str = ACCESS_RENEW_EXTEND_SECONDS_METADATA_KEY,
) -> None:
    """
    If ``extensions`` include ``access.renew.extend.seconds``, set ``mapping[metadata_key]``.

    ``mapping`` may be Kubernetes annotations or Docker container labels.
    """
    if not extensions:
        return
    raw = extensions.get(ACCESS_RENEW_EXTEND_SECONDS_KEY)
    if raw is None:
        return
    s = str(raw).strip()
    if not s:
        return
    mapping[metadata_key] = s


def apply_extensions_to_annotations(
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