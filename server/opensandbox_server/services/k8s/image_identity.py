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

"""Read the primary container identity only from a current workload-owned Pod."""

import re
from typing import Any

from opensandbox_server.services.constants import SANDBOX_SNAPSHOT_ID_LABEL
from opensandbox_server.services.image_identity import registry_image_digest


# Written by the BatchSandbox controller; its status has no selector field.
_BATCH_SANDBOX_NAME_LABEL = "batch-sandbox.sandbox.opensandbox.io/name"
_LABEL_VALUE = re.compile(r"[a-zA-Z0-9](?:[-a-zA-Z0-9_.]{0,61}[a-zA-Z0-9])?")


def _field(value: Any, camel: str, snake: str | None = None) -> Any:
    if isinstance(value, dict):
        return value.get(camel)
    return getattr(value, snake or camel, None)


def workload_image_digest(workload: Any, provider: Any) -> str | None:
    """Return no identity for pools, snapshots, ambiguous Pods, or missing evidence."""
    try:
        metadata = _field(workload, "metadata")
        spec = _field(workload, "spec")
        labels = _field(metadata, "labels") or {}
        if labels.get(SANDBOX_SNAPSHOT_ID_LABEL) or _field(spec, "poolRef", "pool_ref"):
            return None
        if _field(metadata, "deletionTimestamp", "deletion_timestamp"):
            return None
        template = _field(spec, "template") or _field(spec, "podTemplate", "pod_template")
        pod_spec = _field(template, "spec") if template else spec
        containers = _field(pod_spec, "containers")
        if not isinstance(containers, list) or not containers:
            return None
        name = _field(containers[0], "name")
        if not isinstance(name, str) or not name:
            return None

        if _field(workload, "kind") == "Pod":
            pod = workload
        else:
            namespace = _field(metadata, "namespace")
            uid = _field(metadata, "uid")
            selector = _field(_field(workload, "status"), "selector")
            if _field(workload, "kind") == "BatchSandbox":
                workload_name = _field(metadata, "name")
                if not isinstance(workload_name, str) or not _LABEL_VALUE.fullmatch(workload_name):
                    return None
                selector = f"{_BATCH_SANDBOX_NAME_LABEL}={workload_name}"
            if not all(isinstance(value, str) and value for value in (namespace, uid, selector)):
                return None
            pods = provider.k8s_client.list_pods(namespace=namespace, label_selector=selector)
            if not isinstance(pods, list):
                return None
            owned = []
            for candidate in pods:
                pod_metadata = _field(candidate, "metadata")
                if _field(pod_metadata, "namespace") != namespace:
                    continue
                if _field(pod_metadata, "deletionTimestamp", "deletion_timestamp"):
                    continue
                owners = _field(pod_metadata, "ownerReferences", "owner_references") or []
                if any(
                    _field(owner, "uid") == uid and _field(owner, "controller") is True
                    for owner in owners
                ):
                    owned.append(candidate)
            if len(owned) != 1:
                return None
            pod = owned[0]

        statuses = _field(_field(pod, "status"), "containerStatuses", "container_statuses")
        if not isinstance(statuses, list):
            return None
        identities = [
            _field(status, "imageID", "image_id")
            for status in statuses
            if _field(status, "name") == name
        ]
        if len(identities) != 1:
            return None
        return registry_image_digest(identities[0])
    except Exception:
        # Pod status is optional evidence; it must not break create/get/list.
        return None
