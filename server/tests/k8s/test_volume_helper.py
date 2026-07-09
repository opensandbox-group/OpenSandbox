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

import pytest

from opensandbox_server.api.schema import Host, PVC, Volume
from opensandbox_server.services.k8s.volume_helper import apply_volumes_to_pod_spec


def _empty_pod_spec():
    return {
        "containers": [{"name": "main", "volumeMounts": []}],
        "volumes": [],
    }


def _get_pvc_volume_spec(pod_spec, claim_name):
    for v in pod_spec["volumes"]:
        pvc = v.get("persistentVolumeClaim", {})
        if pvc.get("claimName") == claim_name:
            return v
    return None


def _get_mount(pod_spec, mount_path):
    mounts = pod_spec["containers"][0].get("volumeMounts", [])
    return next((m for m in mounts if m["mountPath"] == mount_path), None)


class TestApplyVolumesToPodSpec:
    def test_pvc_rw_no_readonly_in_volume_spec(self):
        """Read-write PVC must not set readOnly on the PVC volume spec."""
        pod_spec = _empty_pod_spec()
        volumes = [
            Volume(
                name="data-vol",
                pvc=PVC(claim_name="my-pvc"),
                mount_path="/mnt/data",
                read_only=False,
            )
        ]
        apply_volumes_to_pod_spec(pod_spec, volumes)

        pvc_vol = _get_pvc_volume_spec(pod_spec, "my-pvc")
        assert pvc_vol is not None
        pvc_src = pvc_vol["persistentVolumeClaim"]
        assert "readOnly" not in pvc_src

        mount = _get_mount(pod_spec, "/mnt/data")
        assert mount is not None
        assert mount["readOnly"] is False

    def test_pvc_readonly_sets_readonly_on_volume_spec_and_mount(self):
        """readOnly=True must propagate to both PVC volume spec and VolumeMount."""
        pod_spec = _empty_pod_spec()
        volumes = [
            Volume(
                name="models-vol",
                pvc=PVC(claim_name="models-pvc"),
                mount_path="/mnt/models",
                read_only=True,
            )
        ]
        apply_volumes_to_pod_spec(pod_spec, volumes)

        pvc_vol = _get_pvc_volume_spec(pod_spec, "models-pvc")
        assert pvc_vol is not None
        pvc_src = pvc_vol["persistentVolumeClaim"]
        assert pvc_src.get("readOnly") is True

        mount = _get_mount(pod_spec, "/mnt/models")
        assert mount is not None
        assert mount["readOnly"] is True

    def test_same_pvc_ro_and_rw_gets_separate_volume_entries(self):
        """Same PVC mounted RO and RW must produce separate pod volume entries."""
        pod_spec = _empty_pod_spec()
        volumes = [
            Volume(
                name="vol-ro",
                pvc=PVC(claim_name="shared-pvc"),
                mount_path="/mnt/ro",
                read_only=True,
            ),
            Volume(
                name="vol-rw",
                pvc=PVC(claim_name="shared-pvc"),
                mount_path="/mnt/rw",
                read_only=False,
            ),
        ]
        apply_volumes_to_pod_spec(pod_spec, volumes)

        pvc_entries = [
            v for v in pod_spec["volumes"]
            if v.get("persistentVolumeClaim", {}).get("claimName") == "shared-pvc"
        ]
        assert len(pvc_entries) == 2

        ro_entry = next(
            v for v in pvc_entries
            if v["persistentVolumeClaim"].get("readOnly") is True
        )
        rw_entry = next(
            v for v in pvc_entries
            if "readOnly" not in v["persistentVolumeClaim"]
        )

        ro_mount = _get_mount(pod_spec, "/mnt/ro")
        rw_mount = _get_mount(pod_spec, "/mnt/rw")

        assert ro_mount is not None
        assert ro_mount["name"] == ro_entry["name"]
        assert ro_mount["readOnly"] is True
        assert rw_mount is not None
        assert rw_mount["name"] == rw_entry["name"]
        assert rw_mount["readOnly"] is False

    def test_host_volume_readonly_sets_readonly_on_mount(self):
        """readOnly=True on a host volume sets the VolumeMount readOnly flag."""
        pod_spec = _empty_pod_spec()
        volumes = [
            Volume(
                name="host-vol",
                host=Host(path="/data/shared"),
                mount_path="/mnt/shared",
                read_only=True,
            )
        ]
        apply_volumes_to_pod_spec(pod_spec, volumes)

        mount = _get_mount(pod_spec, "/mnt/shared")
        assert mount is not None
        assert mount["readOnly"] is True

    def test_conflicts_with_existing_volume_name(self):
        """Volume name that collides with an existing pod volume raises ValueError."""
        pod_spec = _empty_pod_spec()
        pod_spec["volumes"].append({"name": "existing-vol"})
        volumes = [
            Volume(
                name="existing-vol",
                pvc=PVC(claim_name="my-pvc"),
                mount_path="/mnt/data",
            )
        ]
        with pytest.raises(ValueError, match="conflicts with an internal volume"):
            apply_volumes_to_pod_spec(pod_spec, volumes)

    def test_pvc_with_subpath(self):
        """subPath is forwarded to the VolumeMount."""
        pod_spec = _empty_pod_spec()
        volumes = [
            Volume(
                name="sub-vol",
                pvc=PVC(claim_name="big-pvc"),
                mount_path="/mnt/sub",
                sub_path="tenant-a",
            )
        ]
        apply_volumes_to_pod_spec(pod_spec, volumes)

        mount = _get_mount(pod_spec, "/mnt/sub")
        assert mount is not None
        assert mount["subPath"] == "tenant-a"
