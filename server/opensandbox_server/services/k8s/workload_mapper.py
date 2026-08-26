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

import json
import re
from typing import Any, Optional

from opensandbox_server.api.schema import (
    AllocationSummary,
    ImageSpec,
    PlatformSpec,
    Sandbox,
    SandboxStatus,
)
from opensandbox_server.extensions import extract_extensions_from_mapping
from opensandbox_server.services.constants import SANDBOX_ID_LABEL, SANDBOX_SNAPSHOT_ID_LABEL


def _is_opensandbox_label(label_key: str) -> bool:
    return label_key.split("/", 1)[0] == "opensandbox.io"


def _build_sandbox_from_workload(workload: Any, workload_provider: Any) -> Sandbox:
    if isinstance(workload, dict):
        metadata = workload.get("metadata", {})
        spec = workload.get("spec", {})
        labels = metadata.get("labels", {})
        annotations = metadata.get("annotations", {})
        creation_timestamp = metadata.get("creationTimestamp")
    else:
        metadata = workload.metadata
        spec = workload.spec
        labels = metadata.labels or {}
        annotations = metadata.annotations or {}
        creation_timestamp = metadata.creation_timestamp

    sandbox_id = labels.get(SANDBOX_ID_LABEL, "")
    snapshot_id = labels.get(SANDBOX_SNAPSHOT_ID_LABEL)
    expires_at = workload_provider.get_expiration(workload)
    status_info = workload_provider.get_status(workload)
    workload_status = workload.get("status", {}) if isinstance(workload, dict) else workload.status

    user_metadata = {
        k: v for k, v in labels.items() if not _is_opensandbox_label(k)
    }

    image_uri = ""
    entrypoint = []
    if isinstance(workload, dict):
        template = spec.get("template") or spec.get("podTemplate") or {}
        pod_spec = template.get("spec", {})
        containers = pod_spec.get("containers", [])
        if containers:
            container = containers[0]
            image_uri = container.get("image", "")
            entrypoint = container.get("command", [])
    elif hasattr(spec, "containers") and spec.containers:
        container = spec.containers[0]
        image_uri = container.image or ""
        entrypoint = container.command or []

    image_spec = None
    if not snapshot_id:
        image_spec = ImageSpec(uri=image_uri) if image_uri else ImageSpec(uri="unknown")
    platform_spec = _extract_platform_from_workload(workload)
    allocation = _extract_confirmed_pool_allocation(metadata, spec, workload_status)
    return Sandbox(
        id=sandbox_id,
        status=SandboxStatus(
            state=status_info["state"],
            reason=status_info["reason"],
            message=status_info["message"],
            last_transition_at=status_info["last_transition_at"],
        ),
        created_at=creation_timestamp,
        expires_at=expires_at,
        metadata=user_metadata if user_metadata else None,
        extensions=extract_extensions_from_mapping(annotations),
        image=image_spec,
        snapshotId=snapshot_id,
        entrypoint=entrypoint,
        platform=platform_spec,
        allocation=allocation,
    )


_POD_NAME_PATTERN = re.compile(r"^[a-z0-9](?:[-a-z0-9.]{0,251}[a-z0-9])?$")
_ALLOCATION_RELEASE_ANNOTATION_KEYS = (
    "sandbox.opensandbox.io/alloc-release",
    "sandbox.opensandbox.io/alloc-released",
)


def _extract_confirmed_pool_allocation(
    metadata: Any,
    spec: Any,
    status: Any,
) -> Optional[AllocationSummary]:
    """Return a summary only when current pool allocation evidence is complete."""
    pool_ref = _field(spec, "poolRef", "pool_ref")
    if not isinstance(pool_ref, str) or not pool_ref.strip() or pool_ref == "*":
        return None

    if _field(metadata, "deletionTimestamp", "deletion_timestamp"):
        return None
    finalizers = _field(metadata, "finalizers")
    if (
        not isinstance(finalizers, list)
        or "pool.sandbox.opensandbox.io/pool-allocation" not in finalizers
    ):
        return None

    annotations = _field(metadata, "annotations")
    if not isinstance(annotations, dict):
        return None
    raw_allocation = annotations.get("sandbox.opensandbox.io/alloc-status")
    if not isinstance(raw_allocation, str):
        return None
    try:
        annotation = json.loads(raw_allocation)
    except (TypeError, ValueError):
        return None
    if not isinstance(annotation, dict) or annotation.get("poolRef") != pool_ref:
        return None

    pods = annotation.get("pods")
    if (
        not isinstance(pods, list)
        or not pods
        or any(not isinstance(pod, str) or not _POD_NAME_PATTERN.fullmatch(pod) for pod in pods)
        or len(set(pods)) != len(pods)
    ):
        return None
    allocated = _field(status, "allocated")
    if (
        not isinstance(allocated, int)
        or isinstance(allocated, bool)
        or allocated != len(pods)
    ):
        return None
    if _has_released_or_releasing_allocation_pods(annotations, pods):
        return None

    return AllocationSummary(poolRef=pool_ref)


def _has_released_or_releasing_allocation_pods(
    annotations: dict[Any, Any],
    allocation_pods: list[str],
) -> bool:
    """Return whether release state is malformed or includes allocated pods."""
    allocation_pod_names = set(allocation_pods)
    for key in _ALLOCATION_RELEASE_ANNOTATION_KEYS:
        if key not in annotations:
            continue

        raw_release = annotations[key]
        if not isinstance(raw_release, str):
            return True
        try:
            release = json.loads(raw_release)
        except (TypeError, ValueError):
            return True
        if not isinstance(release, dict):
            return True

        released_pods = release.get("pods")
        if (
            not isinstance(released_pods, list)
            or any(
                not isinstance(pod, str)
                or not _POD_NAME_PATTERN.fullmatch(pod)
                for pod in released_pods
            )
            or len(set(released_pods)) != len(released_pods)
        ):
            return True
        if allocation_pod_names.intersection(released_pods):
            return True

    return False


def _field(value: Any, *names: str) -> Any:
    if isinstance(value, dict):
        for name in names:
            if name in value:
                return value[name]
        return None
    for name in names:
        field = getattr(value, name, None)
        if field is not None:
            return field
    return None


def _extract_platform_from_workload(workload: Any) -> Optional[PlatformSpec]:
    if isinstance(workload, dict):
        spec = workload.get("spec") or {}
        template = spec.get("template") or {}
        pod_template = spec.get("podTemplate") or {}
        pod_spec = (
            (template.get("spec") if isinstance(template, dict) else None)
            or (pod_template.get("spec") if isinstance(pod_template, dict) else None)
            or {}
        )
    else:
        spec = getattr(workload, "spec", None)
        template = getattr(spec, "template", None)
        pod_template = getattr(spec, "pod_template", None)
        pod_spec = (
            getattr(template, "spec", None)
            or getattr(pod_template, "spec", None)
            or {}
        )

    node_selector = (
        pod_spec.get("nodeSelector", {})
        if isinstance(pod_spec, dict)
        else getattr(pod_spec, "node_selector", {}) or {}
    )
    if not isinstance(node_selector, dict):
        return None

    os_value = node_selector.get("kubernetes.io/os")
    arch_value = node_selector.get("kubernetes.io/arch")
    os_constraint = os_value if isinstance(os_value, str) and os_value else None
    arch_constraint = arch_value if isinstance(arch_value, str) and arch_value else None

    affinity = (
        pod_spec.get("affinity")
        if isinstance(pod_spec, dict)
        else getattr(pod_spec, "affinity", None)
    )
    if os_constraint is None:
        os_constraint = _extract_platform_value_from_affinity(
            affinity,
            "kubernetes.io/os",
        )
    if arch_constraint is None:
        arch_constraint = _extract_platform_value_from_affinity(
            affinity,
            "kubernetes.io/arch",
        )

    if os_constraint and arch_constraint:
        return PlatformSpec(os=os_constraint, arch=arch_constraint)
    return None


def _extract_platform_value_from_affinity(
    affinity: Any,
    key: str,
) -> Optional[str]:
    if affinity is None:
        return None
    node_affinity = (
        affinity.get("nodeAffinity")
        if isinstance(affinity, dict)
        else getattr(affinity, "node_affinity", None)
    )
    if node_affinity is None:
        return None
    required = (
        node_affinity.get("requiredDuringSchedulingIgnoredDuringExecution")
        if isinstance(node_affinity, dict)
        else getattr(
            node_affinity,
            "required_during_scheduling_ignored_during_execution",
            None,
        )
    )
    if required is None:
        return None
    terms = (
        required.get("nodeSelectorTerms", [])
        if isinstance(required, dict)
        else getattr(required, "node_selector_terms", []) or []
    )
    if not isinstance(terms, list) or not terms:
        return None

    inferred: Optional[str] = None
    for term in terms:
        expressions = (
            term.get("matchExpressions", [])
            if isinstance(term, dict)
            else getattr(term, "match_expressions", []) or []
        )
        if not isinstance(expressions, list):
            return None
        term_value: Optional[str] = None
        for expr in expressions:
            expr_key = (
                expr.get("key")
                if isinstance(expr, dict)
                else getattr(expr, "key", None)
            )
            if expr_key != key:
                continue
            operator = (
                expr.get("operator")
                if isinstance(expr, dict)
                else getattr(expr, "operator", None)
            )
            values = (
                expr.get("values", [])
                if isinstance(expr, dict)
                else getattr(expr, "values", []) or []
            )
            if operator != "In" or not isinstance(values, list) or len(values) != 1:
                return None
            value = values[0]
            if not isinstance(value, str) or not value:
                return None
            term_value = value
            break
        if term_value is None:
            return None
        if inferred is None:
            inferred = term_value
        elif inferred != term_value:
            return None
    return inferred
