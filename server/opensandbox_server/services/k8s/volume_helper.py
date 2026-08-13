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
Volume helper utilities for Kubernetes pod specs.
"""

import json
import logging
from typing import Any, Dict, List

from opensandbox_server.api.schema import Volume

logger = logging.getLogger(__name__)

SUBPATH_INITIALIZER_NAME = "volume-subpath-initializer"
_SUBPATH_INITIALIZER_ROOT = "/opensandbox-subpath-pvcs"


def _get_pvc_source_read_only_policies(volumes: List[Volume]) -> Dict[str, bool]:
    """Keep a PVC source read-only only when every mount of that claim is read-only."""
    policies: Dict[str, bool] = {}
    for vol in volumes:
        if vol.pvc is None:
            continue

        claim_name = vol.pvc.claim_name
        policies[claim_name] = policies.get(claim_name, True) and vol.read_only

    return policies


def apply_volumes_to_pod_spec(
    pod_spec: Dict[str, Any],
    volumes: List[Volume],
) -> None:
    """Apply user-specified volumes to a pod spec in-place."""
    containers = pod_spec.get("containers", [])
    if not containers:
        logger.warning("No containers in pod spec, skipping volume mounts")
        return

    main_container = containers[0]
    mounts = main_container.get("volumeMounts", [])
    pod_volumes = pod_spec.get("volumes", [])

    existing_volume_names = {v.get("name") for v in pod_volumes if isinstance(v, dict)}
    pvc_to_volume_name: Dict[str, str] = {}
    pvc_source_read_only = _get_pvc_source_read_only_policies(volumes)

    for vol in volumes:
        vol_name = vol.name

        if vol_name in existing_volume_names:
            raise ValueError(
                f"Volume name '{vol_name}' conflicts with an internal volume. "
                "Please use a different volume name."
            )

        if vol.pvc is not None:
            pvc_claim_name = vol.pvc.claim_name

            if pvc_claim_name not in pvc_to_volume_name:
                pod_volumes.append({
                    "name": vol_name,
                    "persistentVolumeClaim": {
                        "claimName": pvc_claim_name,
                        "readOnly": pvc_source_read_only[pvc_claim_name],
                    },
                })
                pvc_to_volume_name[pvc_claim_name] = vol_name
                existing_volume_names.add(vol_name)

            mount = {
                "name": pvc_to_volume_name[pvc_claim_name],
                "mountPath": vol.mount_path,
                "readOnly": vol.read_only,
            }
            if vol.sub_path:
                mount["subPath"] = vol.sub_path
            mounts.append(mount)

            logger.info(
                "Added PVC volume '%s' (claim: %s, read_only=%s) mounted at '%s' for sandbox",
                pvc_to_volume_name[pvc_claim_name],
                pvc_claim_name,
                vol.read_only,
                vol.mount_path,
            )
        elif vol.host is not None:
            host_path = vol.host.path

            pod_volumes.append({
                "name": vol_name,
                "hostPath": {
                    "path": host_path,
                    "type": "DirectoryOrCreate",
                },
            })

            mount = {
                "name": vol_name,
                "mountPath": vol.mount_path,
                "readOnly": vol.read_only,
            }
            if vol.sub_path:
                mount["subPath"] = vol.sub_path
            mounts.append(mount)

            logger.info(
                f"Added hostPath volume '{vol_name}' (path: {host_path}) mounted at '{vol.mount_path}' for sandbox"
            )
        else:
            raise ValueError(
                f"Volume '{vol_name}' has no supported backend specified. "
                "Supported backends: pvc, host"
            )

    pod_spec["volumes"] = pod_volumes
    main_container["volumeMounts"] = mounts


def add_subpath_initializer_to_pod_spec(
    pod_spec: Dict[str, Any],
    volumes: List[Volume],
    initializer_image: str,
) -> None:
    """Append the fixed PVC subPath initializer when a request requires it."""
    requested_volumes = [volume for volume in volumes if volume.ensure_sub_path_directory]
    if not requested_volumes:
        return

    if not initializer_image or not initializer_image.strip():
        raise ValueError(
            "runtime.execd_image must be set when ensureSubPathDirectory=true."
        )

    init_containers = pod_spec.get("initContainers", [])
    if not isinstance(init_containers, list):
        raise ValueError("Pod spec initContainers must be a list.")
    if any(
        isinstance(container, dict) and container.get("name") == SUBPATH_INITIALIZER_NAME
        for container in init_containers
    ):
        raise ValueError(
            f"Pod template cannot define reserved init container '{SUBPATH_INITIALIZER_NAME}'."
        )

    pvc_volume_names: Dict[str, str] = {}
    for pod_volume in pod_spec.get("volumes", []):
        if not isinstance(pod_volume, dict):
            continue
        pvc = pod_volume.get("persistentVolumeClaim")
        if isinstance(pvc, dict) and isinstance(pvc.get("claimName"), str):
            pvc_volume_names[pvc["claimName"]] = pod_volume["name"]

    plans_by_claim: Dict[str, Dict[str, Any]] = {}
    for volume in requested_volumes:
        # Volume schema validation guarantees pvc and sub_path are present here.
        assert volume.pvc is not None
        assert volume.sub_path is not None
        claim_name = volume.pvc.claim_name
        if claim_name not in pvc_volume_names:
            raise ValueError(f"PVC '{claim_name}' is missing from the generated pod volumes.")
        plan = plans_by_claim.setdefault(claim_name, {"subPaths": []})
        if volume.sub_path not in plan["subPaths"]:
            plan["subPaths"].append(volume.sub_path)

    plan_entries = []
    root_mounts = []
    for index, (claim_name, plan) in enumerate(plans_by_claim.items()):
        root_mount_path = f"{_SUBPATH_INITIALIZER_ROOT}/{index}"
        plan_entries.append({"mountPath": root_mount_path, "subPaths": plan["subPaths"]})
        root_mounts.append(
            {
                "name": pvc_volume_names[claim_name],
                "mountPath": root_mount_path,
            }
        )

    init_containers.append(
        {
            "name": SUBPATH_INITIALIZER_NAME,
            "image": initializer_image.strip(),
            "command": ["/opensandbox-subpath-initializer"],
            "args": ["--plan-json", json.dumps(plan_entries, separators=(",", ":"))],
            "volumeMounts": root_mounts,
        }
    )
    pod_spec["initContainers"] = init_containers
