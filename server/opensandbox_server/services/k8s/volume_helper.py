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

import logging
from typing import Any, Dict, List, Tuple

from opensandbox_server.api.schema import Volume

logger = logging.getLogger(__name__)


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

    existing_volume_names = {
        v.get("name") for v in pod_volumes if isinstance(v, dict)
    }
    # Key: (claim_name, read_only) so the same PVC can be mounted both RO and RW.
    pvc_to_volume_name: Dict[Tuple[str, bool], str] = {}

    for vol in volumes:
        vol_name = vol.name

        if vol_name in existing_volume_names:
            raise ValueError(
                f"Volume name '{vol_name}' conflicts with an internal volume. "
                "Please use a different volume name."
            )

        if vol.pvc is not None:
            pvc_claim_name = vol.pvc.claim_name
            pvc_key = (pvc_claim_name, vol.read_only)

            if pvc_key not in pvc_to_volume_name:
                pvc_volume: Dict[str, Any] = {
                    "claimName": pvc_claim_name,
                }
                if vol.read_only:
                    pvc_volume["readOnly"] = True
                pod_volumes.append({
                    "name": vol_name,
                    "persistentVolumeClaim": pvc_volume,
                })
                pvc_to_volume_name[pvc_key] = vol_name
                existing_volume_names.add(vol_name)
                logger.info(
                    "Added PVC volume '%s' (claim: %s, readOnly: %s) "
                    "mounted at '%s' for sandbox",
                    vol_name, pvc_claim_name, vol.read_only, vol.mount_path,
                )
            else:
                mapped_name = pvc_to_volume_name[pvc_key]
                if vol_name != mapped_name:
                    logger.warning(
                        "PVC mount request for vol '%s' (claim: %s, readOnly: %s) "
                        "reuses existing pod volume '%s'; requested name '%s' is ignored",
                        vol_name, pvc_claim_name, vol.read_only, mapped_name, vol_name,
                    )

            mount = {
                "name": pvc_to_volume_name[pvc_key],
                "mountPath": vol.mount_path,
                "readOnly": vol.read_only,
            }
            if vol.sub_path:
                mount["subPath"] = vol.sub_path
            mounts.append(mount)
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
                "Added hostPath volume '%s' (path: %s) "
                "mounted at '%s' for sandbox",
                vol_name, host_path, vol.mount_path,
            )
        else:
            raise ValueError(
                f"Volume '{vol_name}' has no supported backend specified. "
                "Supported backends: pvc, host"
            )

    pod_spec["volumes"] = pod_volumes
    main_container["volumeMounts"] = mounts
