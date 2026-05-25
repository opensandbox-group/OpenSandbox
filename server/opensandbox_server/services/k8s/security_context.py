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

"""Kubernetes V1SecurityContext ↔ plain dict helpers for CRD pod specs."""

from typing import Any, Dict, Optional


def build_security_context_from_dict(
    security_context_dict: Dict[str, Any],
) -> Optional[Any]:
    """
    Convert a security context dict to ``V1SecurityContext``.

    Empty dict returns None.
    """
    if not security_context_dict:
        return None

    from kubernetes.client import V1SecurityContext, V1Capabilities

    capabilities = None
    if "capabilities" in security_context_dict:
        caps_dict = security_context_dict["capabilities"]
        add_caps = caps_dict.get("add", [])
        drop_caps = caps_dict.get("drop", [])
        capabilities = V1Capabilities(
            add=add_caps if add_caps else None,
            drop=drop_caps if drop_caps else None,
        )

    privileged = security_context_dict.get("privileged")

    if capabilities is None and privileged is None:
        return None

    return V1SecurityContext(
        capabilities=capabilities,
        privileged=privileged,
    )


def serialize_security_context_to_dict(
    security_context: Optional[Any],
) -> Optional[Dict[str, Any]]:
    """Serialize ``V1SecurityContext`` to a CRD-friendly dict."""
    if not security_context:
        return None

    result: Dict[str, Any] = {}

    if security_context.capabilities:
        caps: Dict[str, Any] = {}
        if security_context.capabilities.add:
            caps["add"] = security_context.capabilities.add
        if security_context.capabilities.drop:
            caps["drop"] = security_context.capabilities.drop
        if caps:
            result["capabilities"] = caps

    if security_context.privileged is not None:
        result["privileged"] = security_context.privileged

    return result if result else None
