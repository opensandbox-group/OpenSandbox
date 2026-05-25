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

"""Unit tests for K8s windows_profile module."""

import pytest

from opensandbox_server.services.k8s.windows_profile import (
    _memory_with_qemu_overhead,
    apply_windows_profile_overrides,
    build_windows_profile_env,
)


class TestMemoryWithQemuOverhead:
    """Tests for _memory_with_qemu_overhead helper."""

    @pytest.mark.parametrize(
        ("input_value", "expected"),
        [
            ("8G", "10Gi"),
            ("16G", "18Gi"),
            ("4Gi", "6Gi"),
            ("8Gb", "10Gi"),
        ],
    )
    def test_gigabyte_units(self, input_value, expected):
        assert _memory_with_qemu_overhead(input_value) == expected

    @pytest.mark.parametrize(
        ("input_value", "expected"),
        [
            ("8192M", "10Gi"),     # 8192/1024 = 8, + 2 = 10
            ("8192Mi", "10Gi"),
            ("4096Mb", "6Gi"),     # 4096/1024 = 4, + 2 = 6
            ("1000Mi", "3Gi"),     # ceil(1000/1024) = 1, + 2 = 3
        ],
    )
    def test_megabyte_units(self, input_value, expected):
        assert _memory_with_qemu_overhead(input_value) == expected

    @pytest.mark.parametrize(
        ("input_value", "expected"),
        [
            ("1T", "1026Gi"),      # 1*1024 + 2 = 1026
            ("1Ti", "1026Gi"),
        ],
    )
    def test_terabyte_units(self, input_value, expected):
        assert _memory_with_qemu_overhead(input_value) == expected

    def test_unrecognized_unit_returns_original(self):
        assert _memory_with_qemu_overhead("8K") == "8K"
        assert _memory_with_qemu_overhead("8Ki") == "8Ki"

    def test_unparseable_value_returns_original(self):
        assert _memory_with_qemu_overhead("invalid") == "invalid"
        assert _memory_with_qemu_overhead("") == ""

    def test_whitespace_tolerance(self):
        assert _memory_with_qemu_overhead(" 8 G ") == "10Gi"


class TestBuildWindowsProfileEnv:
    """Tests for build_windows_profile_env."""

    def test_does_not_inject_kvm_n_by_default(self):
        result = build_windows_profile_env(
            env={"VERSION": "11"},
            resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
        )
        env_dict = {item["name"]: item["value"] for item in result}
        assert "KVM" not in env_dict

    def test_preserves_user_kvm_override(self):
        result = build_windows_profile_env(
            env={"VERSION": "11", "KVM": "N"},
            resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
        )
        env_dict = {item["name"]: item["value"] for item in result}
        assert env_dict["KVM"] == "N"

    def test_includes_user_env_and_resource_derived_env(self):
        result = build_windows_profile_env(
            env={"VERSION": "11", "LANGUAGE": "Chinese"},
            resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
        )
        env_dict = {item["name"]: item["value"] for item in result}
        assert env_dict["VERSION"] == "11"
        assert env_dict["LANGUAGE"] == "Chinese"
        assert env_dict["CPU_CORES"] == "4"
        assert env_dict["RAM_SIZE"] == "8G"
        assert env_dict["DISK_SIZE"] == "64G"


class TestApplyWindowsProfileOverrides:
    """Tests for apply_windows_profile_overrides entrypoint and resource handling."""

    def _make_pod_spec(self):
        return {
            "initContainers": [
                {
                    "name": "execd-installer",
                    "image": "execd:test",
                    "command": ["/bin/sh", "-c"],
                    "args": ["cp ./execd /opt/opensandbox/bin/execd"],
                    "volumeMounts": [
                        {"name": "opensandbox-bin", "mountPath": "/opt/opensandbox/bin"}
                    ],
                }
            ],
            "containers": [
                {
                    "name": "sandbox",
                    "image": "dockurr/windows:latest",
                    "command": ["/opt/opensandbox/bin/bootstrap.sh", "tail", "-f", "/dev/null"],
                    "env": [{"name": "EXECD", "value": "/opt/opensandbox/bin/execd"}],
                    "volumeMounts": [
                        {"name": "opensandbox-bin", "mountPath": "/opt/opensandbox/bin"}
                    ],
                }
            ],
            "volumes": [{"name": "opensandbox-bin", "emptyDir": {}}],
        }

    def test_custom_entrypoint_sets_command(self):
        pod_spec = self._make_pod_spec()
        apply_windows_profile_overrides(
            pod_spec=pod_spec,
            entrypoint=["/bin/sh", "-c", "patch && exec /run/entry.sh"],
            env={"VERSION": "11"},
            resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
        )
        main = pod_spec["containers"][0]
        assert main["command"] == ["/bin/sh", "-c", "patch && exec /run/entry.sh"]
        assert "args" not in main

    def test_default_entrypoint_removes_command(self):
        pod_spec = self._make_pod_spec()
        apply_windows_profile_overrides(
            pod_spec=pod_spec,
            entrypoint=["tail", "-f", "/dev/null"],
            env={"VERSION": "11"},
            resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
        )
        main = pod_spec["containers"][0]
        assert "command" not in main
        assert "args" not in main

    def test_empty_entrypoint_removes_command(self):
        pod_spec = self._make_pod_spec()
        apply_windows_profile_overrides(
            pod_spec=pod_spec,
            entrypoint=[],
            env={"VERSION": "11"},
            resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
        )
        main = pod_spec["containers"][0]
        assert "command" not in main

    def test_resource_limits_sets_resources_with_overhead(self):
        pod_spec = self._make_pod_spec()
        apply_windows_profile_overrides(
            pod_spec=pod_spec,
            entrypoint=["tail", "-f", "/dev/null"],
            env={},
            resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
        )
        main = pod_spec["containers"][0]
        assert main["resources"]["limits"]["cpu"] == "4"
        assert main["resources"]["limits"]["memory"] == "10Gi"
        assert main["resources"]["requests"]["cpu"] == "4"
        assert main["resources"]["requests"]["memory"] == "10Gi"

    def test_empty_resource_limits_removes_resources(self):
        pod_spec = self._make_pod_spec()
        pod_spec["containers"][0]["resources"] = {"limits": {"cpu": "1"}}
        apply_windows_profile_overrides(
            pod_spec=pod_spec,
            entrypoint=["tail", "-f", "/dev/null"],
            env={},
            resource_limits={},
        )
        main = pod_spec["containers"][0]
        assert "resources" not in main

    def test_resource_limits_with_only_disk_removes_resources(self):
        """disk is not a K8s resource, so if only disk is present, no limits are set."""
        pod_spec = self._make_pod_spec()
        apply_windows_profile_overrides(
            pod_spec=pod_spec,
            entrypoint=["tail", "-f", "/dev/null"],
            env={},
            resource_limits={"disk": "64G"},
        )
        main = pod_spec["containers"][0]
        assert "resources" not in main

    def test_sets_privileged_true(self):
        pod_spec = self._make_pod_spec()
        apply_windows_profile_overrides(
            pod_spec=pod_spec,
            entrypoint=["tail", "-f", "/dev/null"],
            env={},
            resource_limits={"cpu": "4", "memory": "8G"},
        )
        main = pod_spec["containers"][0]
        assert main["securityContext"]["privileged"] is True

    def test_sets_restart_policy_always(self):
        pod_spec = self._make_pod_spec()
        pod_spec["restartPolicy"] = "Never"
        apply_windows_profile_overrides(
            pod_spec=pod_spec,
            entrypoint=["tail", "-f", "/dev/null"],
            env={},
            resource_limits={"cpu": "4", "memory": "8G"},
        )
        assert pod_spec["restartPolicy"] == "Always"

    def test_adds_storage_volume_and_mount(self):
        pod_spec = self._make_pod_spec()
        apply_windows_profile_overrides(
            pod_spec=pod_spec,
            entrypoint=["tail", "-f", "/dev/null"],
            env={},
            resource_limits={"cpu": "4", "memory": "8G"},
        )
        volume_names = [v["name"] for v in pod_spec["volumes"]]
        assert "opensandbox-win-storage" in volume_names
        storage_vol = next(v for v in pod_spec["volumes"] if v["name"] == "opensandbox-win-storage")
        assert storage_vol == {"name": "opensandbox-win-storage", "emptyDir": {}}

        main = pod_spec["containers"][0]
        mount_names = [m["name"] for m in main["volumeMounts"]]
        assert "opensandbox-win-storage" in mount_names
        storage_mount = next(m for m in main["volumeMounts"] if m["name"] == "opensandbox-win-storage")
        assert storage_mount["mountPath"] == "/storage"
