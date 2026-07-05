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
from types import SimpleNamespace
from datetime import datetime, timezone
from unittest.mock import MagicMock
from fastapi import HTTPException
from kubernetes.client import ApiException

from opensandbox_server.api.schema import (
    ImageSpec,
    ImageAuth,
    NetworkPolicy,
    NetworkRule,
    PlatformSpec,
)
from opensandbox_server.config import (
    AppConfig,
    EGRESS_MODE_DNS,
    EGRESS_MODE_DNS_NFT,
    EgressConfig,
    ExecdInitResources,
    KubernetesRuntimeConfig,
    RuntimeConfig,
)
from opensandbox_server.services.constants import (
    OPEN_SANDBOX_EGRESS_AUTH_HEADER,
    OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT,
    OPENSANDBOX_RUNTIME_MOUNT_PATH,
    OPENSANDBOX_RUNTIME_VOLUME_NAME,
    SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY,
)
from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider
from opensandbox_server.services.constants import OPENSANDBOX_EGRESS_TOKEN
from opensandbox_server.services.k8s.image_pull_secret_helper import IMAGE_AUTH_SECRET_PREFIX
from opensandbox_server.services.k8s.volume_helper import apply_volumes_to_pod_spec


def _app_config_with_template(template_file_path: str) -> AppConfig:
    """Build an AppConfig with a batchsandbox_template_file set."""
    return AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
        kubernetes=KubernetesRuntimeConfig(
            namespace="test-ns",
            batchsandbox_template_file=template_file_path,
        ),
    )


def _app_config_with_execd_resources(execd_init_resources: ExecdInitResources) -> AppConfig:
    """Build an AppConfig with execd_init_resources set."""
    return AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
        kubernetes=KubernetesRuntimeConfig(
            namespace="test-ns",
            execd_init_resources=execd_init_resources,
        ),
    )


def _app_config_with_image_pull_policy(image_pull_policy: str) -> AppConfig:
    """Build an AppConfig with image_pull_policy set."""
    return AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
        kubernetes=KubernetesRuntimeConfig(
            namespace="test-ns",
            image_pull_policy=image_pull_policy,
        ),
    )


def _app_config_with_egress_disable_ipv6(disable_ipv6: bool = True) -> AppConfig:
    """Build an AppConfig with ``egress.disable_ipv6`` set (privileged execd init when egress is used)."""
    return AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
        kubernetes=KubernetesRuntimeConfig(namespace="test-ns"),
        egress=EgressConfig(disable_ipv6=disable_ipv6),
    )


class TestBatchSandboxProvider:
    # ===== Initialization Tests =====

    def test_init_without_template_creates_provider(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)

        assert provider.k8s_client == mock_k8s_client
        assert provider.template_manager._template is None
        assert provider.group == "sandbox.opensandbox.io"
        assert provider.version == "v1alpha1"
        assert provider.plural == "batchsandboxes"

    def test_init_with_template_loads_template(self, mock_k8s_client, tmp_path):
        template_file = tmp_path / "template.yaml"
        template_file.write_text("spec:\n  replicas: 1")

        provider = BatchSandboxProvider(
            mock_k8s_client, _app_config_with_template(str(template_file))
        )

        assert provider.template_manager._template is not None

    def test_init_sets_crd_constants_correctly(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)

        assert provider.group == "sandbox.opensandbox.io"
        assert provider.version == "v1alpha1"
        assert provider.plural == "batchsandboxes"

    # ===== Workload Creation Tests =====

    def test_create_workload_builds_correct_manifest(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        expires_at = datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)

        result = provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={"FOO": "bar"},
            resource_limits={"cpu": "1", "memory": "1Gi"},
            labels={"opensandbox.io/id": "test-id"},
            expires_at=expires_at,
            execd_image="execd:latest",
        )
        
        assert result == {"name": "test-id", "uid": "test-uid", "apiVersion": "sandbox.opensandbox.io/v1alpha1", "kind": "BatchSandbox"}
        
        # Verify API call
        call_args = mock_k8s_client.create_custom_object.call_args
        body = call_args.kwargs["body"]

        assert body["apiVersion"] == "sandbox.opensandbox.io/v1alpha1"
        assert body["kind"] == "BatchSandbox"
        assert body["metadata"]["name"] == "test-id"
        assert body["metadata"]["namespace"] == "test-ns"
        assert body["spec"]["replicas"] == 1
        assert body["spec"]["expireTime"] == "2025-12-31T10:00:00+00:00"
        assert "template" in body["spec"]
        assert "initContainers" in body["spec"]["template"]["spec"]
        assert "containers" in body["spec"]["template"]["spec"]
        assert "volumes" in body["spec"]["template"]["spec"]

    def test_create_workload_injects_platform_node_selector(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={"cpu": "1", "memory": "1Gi"},
            labels={"opensandbox.io/id": "test-id"},
            expires_at=None,
            execd_image="execd:latest",
            platform=PlatformSpec(os="linux", arch="arm64"),
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        node_selector = body["spec"]["template"]["spec"]["nodeSelector"]
        assert node_selector["kubernetes.io/os"] == "linux"
        assert node_selector["kubernetes.io/arch"] == "arm64"

    def test_create_workload_windows_profile_uses_windows_runtime_shape(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="dockurr/windows:latest"),
            entrypoint=["cmd", "/c", "echo hello"],
            env={"VERSION": "11"},
            resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
            labels={"opensandbox.io/id": "test-id"},
            expires_at=None,
            execd_image="execd:latest",
            platform=PlatformSpec(os="windows", arch="amd64"),
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]

        # windows profile should enforce requested arch, but not force os=windows.
        node_selector = pod_spec.get("nodeSelector", {})
        assert node_selector["kubernetes.io/arch"] == "amd64"
        assert "kubernetes.io/os" not in node_selector

        init_container = pod_spec["initContainers"][0]
        assert init_container["command"] == ["/bin/sh", "-c"]
        assert "install.bat" in init_container["args"][0]
        assert "execd.exe" in init_container["args"][0]

        main_container = pod_spec["containers"][0]
        assert main_container["command"] == ["cmd", "/c", "echo hello"]
        # Resources include QEMU memory overhead (8G + 2Gi overhead = 10Gi)
        assert main_container["resources"]["limits"]["cpu"] == "4"
        assert main_container["resources"]["limits"]["memory"] == "10Gi"

        env_dict = {item["name"]: item["value"] for item in main_container.get("env", [])}
        assert env_dict["VERSION"] == "11"
        assert env_dict["CPU_CORES"] == "4"
        assert env_dict["RAM_SIZE"] == "8G"
        assert env_dict["DISK_SIZE"] == "64G"
        assert env_dict["USER_PORTS"] == "44772,8080,3389,8006"

        volume_names = {volume["name"] for volume in pod_spec.get("volumes", [])}
        assert "opensandbox-win-oem" in volume_names
        assert "opensandbox-win-kvm" in volume_names
        assert "opensandbox-win-tun" in volume_names

    def test_create_workload_windows_profile_default_entrypoint_uses_image_entrypoint(
        self, mock_k8s_client
    ):
        """When entrypoint is the SDK default, command is removed so image ENTRYPOINT runs."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="dockurr/windows:latest"),
            entrypoint=["tail", "-f", "/dev/null"],
            env={"VERSION": "11"},
            resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
            labels={"opensandbox.io/id": "test-id"},
            expires_at=None,
            execd_image="execd:latest",
            platform=PlatformSpec(os="windows", arch="amd64"),
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]
        main_container = pod_spec["containers"][0]
        # No command set - image default ENTRYPOINT will be used
        assert "command" not in main_container
        assert "args" not in main_container

    def test_create_workload_windows_profile_merges_user_ports(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="dockurr/windows:latest"),
            entrypoint=["cmd", "/c", "echo hello"],
            env={"VERSION": "11", "USER_PORTS": "3000,44772"},
            resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
            labels={"opensandbox.io/id": "test-id"},
            expires_at=None,
            execd_image="execd:latest",
            platform=PlatformSpec(os="windows", arch="amd64"),
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]
        main_container = pod_spec["containers"][0]
        env_dict = {item["name"]: item["value"] for item in main_container.get("env", [])}
        assert env_dict["USER_PORTS"] == "3000,44772,8080,3389,8006"

    def test_create_workload_windows_profile_rejects_arch_conflict_with_template_selector(
        self, mock_k8s_client, tmp_path
    ):
        template_file = tmp_path / "template.yaml"
        template_file.write_text(
            """
spec:
  template:
    spec:
      nodeSelector:
        kubernetes.io/arch: arm64
"""
        )
        provider = BatchSandboxProvider(
            mock_k8s_client, _app_config_with_template(str(template_file))
        )

        with pytest.raises(ValueError, match="platform conflict with template nodeSelector"):
            provider.create_workload(
                sandbox_id="test-id",
                namespace="test-ns",
                image_spec=ImageSpec(uri="dockurr/windows:latest"),
                entrypoint=["cmd", "/c", "echo hello"],
                env={"VERSION": "11"},
                resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
                labels={"opensandbox.io/id": "test-id"},
                expires_at=None,
                execd_image="execd:latest",
                platform=PlatformSpec(os="windows", arch="amd64"),
            )

    def test_create_workload_rejects_platform_conflict_with_template_selector(
        self, mock_k8s_client, tmp_path
    ):
        template_file = tmp_path / "template.yaml"
        template_file.write_text(
            """
spec:
  template:
    spec:
      nodeSelector:
        kubernetes.io/os: linux
        kubernetes.io/arch: amd64
"""
        )
        provider = BatchSandboxProvider(
            mock_k8s_client, _app_config_with_template(str(template_file))
        )

        with pytest.raises(ValueError, match="platform conflict with template nodeSelector"):
            provider.create_workload(
                sandbox_id="test-id",
                namespace="test-ns",
                image_spec=ImageSpec(uri="python:3.11"),
                entrypoint=["/bin/bash"],
                env={},
                resource_limits={"cpu": "1", "memory": "1Gi"},
                labels={"opensandbox.io/id": "test-id"},
                expires_at=None,
                execd_image="execd:latest",
                platform=PlatformSpec(os="linux", arch="arm64"),
            )

    def test_create_workload_rejects_platform_conflict_with_template_node_affinity(
        self, mock_k8s_client, tmp_path
    ):
        template_file = tmp_path / "template.yaml"
        template_file.write_text(
            """
spec:
  template:
    spec:
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions:
                  - key: kubernetes.io/arch
                    operator: In
                    values: ["amd64"]
"""
        )
        provider = BatchSandboxProvider(
            mock_k8s_client, _app_config_with_template(str(template_file))
        )

        with pytest.raises(ValueError, match="platform conflict with template nodeAffinity"):
            provider.create_workload(
                sandbox_id="test-id",
                namespace="test-ns",
                image_spec=ImageSpec(uri="python:3.11"),
                entrypoint=["/bin/bash"],
                env={},
                resource_limits={"cpu": "1", "memory": "1Gi"},
                labels={"opensandbox.io/id": "test-id"},
                expires_at=None,
                execd_image="execd:latest",
                platform=PlatformSpec(os="linux", arch="arm64"),
            )

    def test_create_workload_builds_execd_init_container(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test", "uid": "uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:test",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        init_container = body["spec"]["template"]["spec"]["initContainers"][0]

        assert init_container["name"] == "execd-installer"
        assert init_container["image"] == "execd:test"
        assert init_container["command"] == ["/bin/sh", "-c"]
        assert "bootstrap.sh" in init_container["args"][0]
        assert init_container["volumeMounts"][0]["name"] == "opensandbox-bin"
        # No resources configured: resources field should be absent
        assert "resources" not in init_container

    def test_create_workload_init_container_with_configured_resources(self, mock_k8s_client):
        provider = BatchSandboxProvider(
            mock_k8s_client,
            _app_config_with_execd_resources(
                ExecdInitResources(
                    limits={"cpu": "100m", "memory": "128Mi"},
                    requests={"cpu": "50m", "memory": "64Mi"},
                )
            ),
        )
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test", "uid": "uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:test",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        init_container = body["spec"]["template"]["spec"]["initContainers"][0]
        assert init_container["resources"]["limits"] == {"cpu": "100m", "memory": "128Mi"}
        assert init_container["resources"]["requests"] == {"cpu": "50m", "memory": "64Mi"}

    def test_create_workload_sets_configured_image_pull_policy(self, mock_k8s_client):
        provider = BatchSandboxProvider(
            mock_k8s_client,
            _app_config_with_image_pull_policy("Always"),
        )
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test", "uid": "uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:test",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        main_container = body["spec"]["template"]["spec"]["containers"][0]
        assert main_container["imagePullPolicy"] == "Always"

    def test_create_workload_wraps_entrypoint_with_bootstrap(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-test", "uid": "uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/usr/bin/python", "app.py"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        main_container = body["spec"]["template"]["spec"]["containers"][0]

        assert main_container["command"] == [
            "/opt/opensandbox/bootstrap.sh",
            "/usr/bin/python",
            "app.py",
        ]

    def test_create_workload_converts_env_to_list(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-test", "uid": "uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={"FOO": "bar", "BAZ": "qux"},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        env_vars = body["spec"]["template"]["spec"]["containers"][0]["env"]

        # Should have user env vars plus EXECD
        assert len(env_vars) == 3
        env_dict = {e["name"]: e["value"] for e in env_vars}
        assert env_dict["FOO"] == "bar"
        assert env_dict["BAZ"] == "qux"
        # Verify EXECD is automatically injected
        assert env_dict["EXECD"] == "/opt/opensandbox/execd"

    def test_create_workload_merges_template_volumes_and_mounts(self, mock_k8s_client, tmp_path):
        template_file = tmp_path / "template.yaml"
        template_file.write_text(
            """
spec:
  template:
    spec:
      volumes:
        - name: sandbox-shared-data
          emptyDir: {}
      containers:
        - name: sandbox
          image: ubuntu:latest
          volumeMounts:
            - name: sandbox-shared-data
              mountPath: /data
"""
        )
        provider = BatchSandboxProvider(
            mock_k8s_client, _app_config_with_template(str(template_file))
        )
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-test", "uid": "uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        spec = body["spec"]["template"]["spec"]

        volume_names = [v["name"] for v in spec["volumes"]]
        assert "sandbox-shared-data" in volume_names
        assert "opensandbox-bin" in volume_names

        # Runtime container should stay intact (template image should not override)
        container = spec["containers"][0]
        assert container["name"] == "sandbox"
        assert container["image"] == "python:3.11"

        mount_names = [m["name"] for m in container["volumeMounts"]]
        assert "sandbox-shared-data" in mount_names
        assert "opensandbox-bin" in mount_names

    def test_create_workload_dedupes_template_volume_and_mount_names(
        self, mock_k8s_client, tmp_path
    ):
        template_file = tmp_path / "template.yaml"
        template_file.write_text(
            """
spec:
  template:
    spec:
      volumes:
        - name: opensandbox-bin
          emptyDir: {}
        - name: sandbox-shared-data
          emptyDir: {}
      containers:
        - name: sandbox
          volumeMounts:
            - name: opensandbox-bin
              mountPath: /opt/opensandbox
            - name: sandbox-shared-data
              mountPath: /data
"""
        )
        provider = BatchSandboxProvider(
            mock_k8s_client, _app_config_with_template(str(template_file))
        )
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-test", "uid": "uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        spec = body["spec"]["template"]["spec"]

        volume_names = [v["name"] for v in spec["volumes"]]
        assert volume_names.count("opensandbox-bin") == 1
        assert "sandbox-shared-data" in volume_names

        mount_names = [m["name"] for m in spec["containers"][0]["volumeMounts"]]
        assert mount_names.count("opensandbox-bin") == 1
        assert "sandbox-shared-data" in mount_names

    def test_create_workload_sets_resource_limits_and_requests(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-test", "uid": "uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={"cpu": "1", "memory": "1Gi"},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        resources = body["spec"]["template"]["spec"]["containers"][0]["resources"]

        assert resources["limits"] == {"cpu": "1", "memory": "1Gi"}
        assert resources["requests"] == {"cpu": "1", "memory": "1Gi"}

    def test_create_workload_uses_separate_resource_requests(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-test", "uid": "uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={"cpu": "2", "memory": "2Gi"},
            resource_requests={"cpu": "500m", "memory": "512Mi"},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        resources = body["spec"]["template"]["spec"]["containers"][0]["resources"]

        assert resources["limits"] == {"cpu": "2", "memory": "2Gi"}
        assert resources["requests"] == {"cpu": "500m", "memory": "512Mi"}

    def test_create_workload_handles_empty_resource_limits(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-test", "uid": "uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        container = body["spec"]["template"]["spec"]["containers"][0]

        assert "resources" not in container

    def test_create_workload_translates_gpu_to_nvidia_extended_resource(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-test", "uid": "uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={"cpu": "1", "memory": "1Gi", "gpu": "2"},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        resources = body["spec"]["template"]["spec"]["containers"][0]["resources"]

        assert resources["limits"]["nvidia.com/gpu"] == "2"
        assert resources["requests"]["nvidia.com/gpu"] == "2"
        # Raw key must not leak through as an unknown extended resource.
        assert "gpu" not in resources["limits"]
        assert "gpu" not in resources["requests"]

    def test_create_workload_without_gpu_omits_nvidia_extended_resource(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-test", "uid": "uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={"cpu": "1", "memory": "1Gi"},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        resources = body["spec"]["template"]["spec"]["containers"][0]["resources"]

        assert "nvidia.com/gpu" not in resources["limits"]
        assert "nvidia.com/gpu" not in resources["requests"]

    def test_create_workload_rejects_gpu_all_sentinel(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)

        with pytest.raises(HTTPException) as excinfo:
            provider.create_workload(
                sandbox_id="test-id",
                namespace="test-ns",
                image_spec=ImageSpec(uri="python:3.11"),
                entrypoint=["/bin/bash"],
                env={},
                resource_limits={"cpu": "1", "gpu": "all"},
                labels={},
                expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
                execd_image="execd:latest",
            )
        assert excinfo.value.status_code == 400

    # ===== Workload Query Tests =====

    def test_get_workload_finds_existing_sandbox(
        self, mock_k8s_client, mock_batchsandbox_list_response
    ):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = mock_batchsandbox_list_response["items"][0]

        result = provider.get_workload("test-id", "test-ns")

        assert result is not None
        assert result["metadata"]["name"] == "test-id"

    def test_get_workload_returns_none_when_not_found(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = None

        result = provider.get_workload("test-id", "test-ns")

        assert result is None

    def test_get_workload_falls_back_to_legacy_name(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.side_effect = [
            None,
            {"metadata": {"name": "sandbox-test-id"}},
        ]

        result = provider.get_workload("test-id", "test-ns")

        assert result["metadata"]["name"] == "sandbox-test-id"
        assert mock_k8s_client.get_custom_object.call_args_list[0].kwargs["name"] == "test-id"
        assert (
            mock_k8s_client.get_custom_object.call_args_list[1].kwargs["name"] == "sandbox-test-id"
        )

    def test_get_workload_handles_404_gracefully(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)

        mock_k8s_client.get_custom_object.return_value = None

        result = provider.get_workload("test-id", "test-ns")

        assert result is None

    def test_get_workload_reraises_non_404_exceptions(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)

        # Mock 500 exception
        error = ApiException(status=500)
        mock_k8s_client.get_custom_object.side_effect = error

        with pytest.raises(ApiException) as exc_info:
            provider.get_workload("test-id", "test-ns")

        assert exc_info.value.status == 500

    def test_get_workload_prefers_informer_cache(self, mock_k8s_client):
        cached = {"metadata": {"name": "test-id"}}
        mock_k8s_client.get_custom_object.return_value = cached

        provider = BatchSandboxProvider(mock_k8s_client)

        result = provider.get_workload("test-id", "test-ns")

        assert result == cached
        mock_k8s_client.get_custom_object.assert_called()

    def test_get_workload_logs_unexpected_errors(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.side_effect = RuntimeError("Unexpected")

        with pytest.raises(RuntimeError, match="Unexpected"):
            provider.get_workload("test-id", "test-ns")

    def test_create_workload_updates_informer_cache(self, mock_k8s_client):
        created_body = {"metadata": {"name": "test-id", "uid": "test-uid"}}
        mock_k8s_client.create_custom_object.return_value = created_body

        provider = BatchSandboxProvider(mock_k8s_client)

        expires_at = datetime(2025, 12, 31, tzinfo=timezone.utc)

        result = provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={"FOO": "bar"},
            resource_limits={"cpu": "1", "memory": "1Gi"},
            labels={"opensandbox.io/id": "test-id"},
            expires_at=expires_at,
            execd_image="execd:latest",
        )

        assert result == {"name": "test-id", "uid": "test-uid", "apiVersion": "sandbox.opensandbox.io/v1alpha1", "kind": "BatchSandbox"}
    
    # ===== Workload List Tests =====

    def test_list_workloads_returns_items(self, mock_k8s_client, mock_batchsandbox_list_response):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.list_custom_objects.return_value = mock_batchsandbox_list_response["items"]

        result = provider.list_workloads("test-ns", "opensandbox.io/id")

        assert len(result) == 1
        assert result[0]["metadata"]["name"] == "test-id"

    def test_list_workloads_returns_empty_on_404(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.list_custom_objects.return_value = []

        result = provider.list_workloads("test-ns", "opensandbox.io/id")

        assert result == []

    # ===== Workload Deletion Tests =====

    def test_delete_workload_deletes_existing_sandbox(
        self, mock_k8s_client, mock_batchsandbox_list_response
    ):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = mock_batchsandbox_list_response["items"][0]

        provider.delete_workload("test-id", "test-ns")

        mock_k8s_client.delete_custom_object.assert_called_once_with(
            group="sandbox.opensandbox.io",
            version="v1alpha1",
            namespace="test-ns",
            plural="batchsandboxes",
            name="test-id",
            grace_period_seconds=0,
        )

    def test_delete_workload_raises_when_not_found(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = None

        with pytest.raises(Exception) as exc_info:
            provider.delete_workload("test-id", "test-ns")

        assert "not found" in str(exc_info.value)

    def test_delete_workload_sets_grace_period_zero(
        self, mock_k8s_client, mock_batchsandbox_list_response
    ):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = mock_batchsandbox_list_response["items"][0]

        provider.delete_workload("test-id", "test-ns")

        call_kwargs = mock_k8s_client.delete_custom_object.call_args.kwargs
        assert call_kwargs["grace_period_seconds"] == 0

    # ===== Expiration Time Management Tests =====

    def test_update_expiration_patches_spec(self, mock_k8s_client, mock_batchsandbox_list_response):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = mock_batchsandbox_list_response["items"][0]

        expires_at = datetime(2025, 12, 31, 0, 0, 0, tzinfo=timezone.utc)
        provider.update_expiration("test-id", "test-ns", expires_at)

        call_kwargs = mock_k8s_client.patch_custom_object.call_args.kwargs
        assert call_kwargs["body"] == {"spec": {"expireTime": "2025-12-31T00:00:00+00:00"}}

    def test_get_expiration_parses_iso_format(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {"spec": {"expireTime": "2025-12-31T10:00:00+00:00"}}

        result = provider.get_expiration(workload)

        assert result == datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)

    def test_get_expiration_handles_z_suffix(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {"spec": {"expireTime": "2025-12-31T10:00:00Z"}}

        result = provider.get_expiration(workload)

        assert result == datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)

    def test_get_expiration_returns_none_on_invalid_format(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {"spec": {"expireTime": "invalid-date"}}

        # Should return None and not raise exception
        result = provider.get_expiration(workload)

        assert result is None

    def test_get_expiration_returns_none_when_missing(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {"spec": {}}

        result = provider.get_expiration(workload)

        assert result is None

    # ===== Status Retrieval Tests =====

    def test_get_status_running_with_ip(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {
            "status": {"replicas": 1, "ready": 1, "allocated": 1},
            "metadata": {
                "annotations": {"sandbox.opensandbox.io/endpoints": '["10.0.0.1"]'},
                "creationTimestamp": "2025-12-24T10:00:00Z",
            },
        }

        result = provider.get_status(workload)

        assert result["state"] == "Running"
        assert result["reason"] == "POD_READY_WITH_IP"
        assert "IP" in result["message"]

    def test_get_status_allocated_with_ip_not_ready(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {
            "status": {"replicas": 1, "ready": 0, "allocated": 1},
            "metadata": {
                "annotations": {"sandbox.opensandbox.io/endpoints": '["10.0.0.1"]'},
                "creationTimestamp": "2025-12-24T10:00:00Z",
            },
        }

        result = provider.get_status(workload)

        assert result["state"] == "Allocated"
        assert result["reason"] == "IP_ASSIGNED"

    def test_get_status_pending_scheduled(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {
            "status": {"replicas": 1, "ready": 0, "allocated": 1},
            "metadata": {"creationTimestamp": "2025-12-24T10:00:00Z"},
        }

        result = provider.get_status(workload)

        assert result["state"] == "Pending"
        assert result["reason"] == "POD_SCHEDULED"

    def test_get_status_pending_when_endpoints_invalid_json(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {
            "status": {"replicas": 1, "ready": 0, "allocated": 1},
            "metadata": {
                "annotations": {"sandbox.opensandbox.io/endpoints": "invalid-json"},
                "creationTimestamp": "2025-12-24T10:00:00Z",
            },
        }

        result = provider.get_status(workload)

        assert result["state"] == "Pending"
        assert result["reason"] == "POD_SCHEDULED"

    def test_get_status_pending_when_endpoints_empty_array(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {
            "status": {"replicas": 1, "ready": 0, "allocated": 1},
            "metadata": {
                "annotations": {"sandbox.opensandbox.io/endpoints": "[]"},
                "creationTimestamp": "2025-12-24T10:00:00Z",
            },
        }

        result = provider.get_status(workload)

        assert result["state"] == "Pending"
        assert result["reason"] == "POD_SCHEDULED"

    def test_get_status_pending_unallocated(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {
            "status": {"replicas": 1, "ready": 0, "allocated": 0},
            "metadata": {"creationTimestamp": "2025-12-24T10:00:00Z"},
        }

        result = provider.get_status(workload)

        assert result["state"] == "Pending"
        assert result["reason"] == "BATCHSANDBOX_PENDING"

    def test_get_status_returns_failed_when_pod_unschedulable(self):
        mock_k8s_client = MagicMock()
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.list_pods.return_value = [
            SimpleNamespace(
                status=SimpleNamespace(
                    phase="Pending",
                    pod_ip=None,
                    conditions=[
                        SimpleNamespace(
                            type="PodScheduled",
                            status="False",
                            reason="Unschedulable",
                            message="0/1 nodes are available: 1 node(s) didn't match Pod's node affinity.",
                        )
                    ],
                )
            )
        ]
        workload = {
            "spec": {
                "template": {
                    "spec": {
                        "nodeSelector": {
                            "kubernetes.io/os": "linux",
                            "kubernetes.io/arch": "arm64",
                        }
                    }
                }
            },
            "status": {
                "replicas": 1,
                "ready": 0,
                "allocated": 0,
                "selector": "app=sandbox",
            },
            "metadata": {
                "namespace": "test-ns",
                "creationTimestamp": "2025-12-24T10:00:00Z",
            },
        }

        result = provider.get_status(workload)

        assert result["state"] == "Failed"
        assert result["reason"] == "POD_PLATFORM_UNSCHEDULABLE"
        assert "didn't match Pod's node affinity" in result["message"]

    def test_get_status_keeps_pending_for_generic_failed_scheduling(self):
        mock_k8s_client = MagicMock()
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.list_pods.return_value = [
            SimpleNamespace(
                status=SimpleNamespace(
                    phase="Pending",
                    pod_ip=None,
                    conditions=[
                        SimpleNamespace(
                            type="PodScheduled",
                            status="False",
                            reason="Unschedulable",
                            message="0/1 nodes are available: 1 Insufficient cpu.",
                        )
                    ],
                )
            )
        ]
        workload = {
            "status": {
                "replicas": 1,
                "ready": 0,
                "allocated": 0,
                "selector": "app=sandbox",
            },
            "metadata": {
                "namespace": "test-ns",
                "creationTimestamp": "2025-12-24T10:00:00Z",
            },
        }

        result = provider.get_status(workload)

        assert result["state"] == "Pending"
        assert result["reason"] == "BATCHSANDBOX_PENDING"

    def test_get_status_keeps_pending_when_non_platform_affinity_mismatch(self):
        mock_k8s_client = MagicMock()
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.list_pods.return_value = [
            SimpleNamespace(
                status=SimpleNamespace(
                    phase="Pending",
                    pod_ip=None,
                    conditions=[
                        SimpleNamespace(
                            type="PodScheduled",
                            status="False",
                            reason="Unschedulable",
                            message="0/1 nodes are available: 1 node(s) didn't match Pod's node affinity.",
                        )
                    ],
                )
            )
        ]
        workload = {
            "spec": {
                "template": {
                    "spec": {
                        "nodeSelector": {
                            "kubernetes.io/os": "linux",
                            "kubernetes.io/arch": "arm64",
                            "zone": "cn-hz-a",
                        }
                    }
                }
            },
            "status": {
                "replicas": 1,
                "ready": 0,
                "allocated": 0,
                "selector": "app=sandbox",
            },
            "metadata": {
                "namespace": "test-ns",
                "creationTimestamp": "2025-12-24T10:00:00Z",
            },
        }

        result = provider.get_status(workload)

        assert result["state"] == "Pending"
        assert result["reason"] == "BATCHSANDBOX_PENDING"

    def test_get_status_keeps_pending_for_mixed_capacity_and_affinity_message(self):
        mock_k8s_client = MagicMock()
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.list_pods.return_value = [
            SimpleNamespace(
                status=SimpleNamespace(
                    phase="Pending",
                    pod_ip=None,
                    conditions=[
                        SimpleNamespace(
                            type="PodScheduled",
                            status="False",
                            reason="Unschedulable",
                            message=(
                                "0/2 nodes are available: 1 Insufficient cpu, "
                                "1 node(s) didn't match Pod's node affinity/selector."
                            ),
                        )
                    ],
                )
            )
        ]
        workload = {
            "spec": {
                "template": {
                    "spec": {
                        "nodeSelector": {
                            "kubernetes.io/os": "linux",
                            "kubernetes.io/arch": "arm64",
                        }
                    }
                }
            },
            "status": {
                "replicas": 1,
                "ready": 0,
                "allocated": 0,
                "selector": "app=sandbox",
            },
            "metadata": {
                "namespace": "test-ns",
                "creationTimestamp": "2025-12-24T10:00:00Z",
            },
        }

        result = provider.get_status(workload)

        assert result["state"] == "Pending"
        assert result["reason"] == "BATCHSANDBOX_PENDING"

    # ===== Endpoint Information Tests =====

    def test_get_endpoint_info_parses_json_annotation(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {
            "metadata": {"annotations": {"sandbox.opensandbox.io/endpoints": '["10.0.0.1"]'}}
        }

        result = provider.get_endpoint_info(workload, 8080, "sandbox-123")

        assert result.endpoint == "10.0.0.1:8080"
        assert result.headers is None

    def test_get_endpoint_info_uses_first_ip(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {
            "metadata": {
                "annotations": {"sandbox.opensandbox.io/endpoints": '["10.0.0.1", "10.0.0.2"]'}
            }
        }

        result = provider.get_endpoint_info(workload, 8080, "sandbox-123")

        assert result.endpoint == "10.0.0.1:8080"
        assert result.headers is None

    def test_get_endpoint_info_returns_none_when_missing(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {"metadata": {"annotations": {}}}

        result = provider.get_endpoint_info(workload, 8080, "sandbox-123")

        assert result is None

    def test_get_endpoint_info_returns_none_on_invalid_json(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {
            "metadata": {"annotations": {"sandbox.opensandbox.io/endpoints": "invalid-json"}}
        }

        result = provider.get_endpoint_info(workload, 8080, "sandbox-123")

        assert result is None

    def test_get_endpoint_info_returns_none_on_empty_array(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {"metadata": {"annotations": {"sandbox.opensandbox.io/endpoints": "[]"}}}

        result = provider.get_endpoint_info(workload, 8080, "sandbox-123")

        assert result is None

    # ===== Pool-based Creation Tests =====

    def test_create_workload_poolref_ignores_image_spec(self, mock_k8s_client):
        """
        Test that pool-based creation ignores image_spec parameter.

        Pool already defines the image, so image_spec is not used even if provided.
        This verifies backward compatibility - no error is raised.
        """
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-test-id", "uid": "test-uid"}
        }

        result = provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["python", "app.py"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
            extensions={"poolRef": "my-pool"},
        )

        # Should succeed and return workload info
        assert result == {"name": "sandbox-test-id", "uid": "test-uid", "apiVersion": "sandbox.opensandbox.io/v1alpha1", "kind": "BatchSandbox"}
        
        # Verify poolRef is used
        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        assert body["spec"]["poolRef"] == "my-pool"

    def test_create_workload_poolref_ignores_resource_limits(self, mock_k8s_client):
        """
        Test that pool-based creation ignores resource_limits parameter.

        Pool already defines the resources, so resource_limits is not used even if provided.
        This verifies backward compatibility - no error is raised.
        """
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-test-id", "uid": "test-uid"}
        }

        result = provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri=""),
            entrypoint=["python", "app.py"],
            env={},
            resource_limits={"cpu": "1", "memory": "1Gi"},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
            extensions={"poolRef": "my-pool"},
        )

        # Should succeed and return workload info
        assert result == {"name": "sandbox-test-id", "uid": "test-uid", "apiVersion": "sandbox.opensandbox.io/v1alpha1", "kind": "BatchSandbox"}
        
        # Verify poolRef is used
        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        assert body["spec"]["poolRef"] == "my-pool"

    def test_create_workload_poolref_allows_entrypoint_and_env(self, mock_k8s_client):
        """
        Test that pool-based creation allows customizing entrypoint and env.

        Verifies taskTemplate structure is correctly generated with user's entrypoint and env.
        """
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-test-id", "uid": "test-uid"}
        }

        result = provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri=""),
            entrypoint=["python", "app.py"],
            env={"FOO": "bar"},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
            extensions={"poolRef": "my-pool"},
        )
        
        assert result == {"name": "sandbox-test-id", "uid": "test-uid", "apiVersion": "sandbox.opensandbox.io/v1alpha1", "kind": "BatchSandbox"}
        
        # Verify the call
        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        assert body["spec"]["poolRef"] == "my-pool"
        assert "taskTemplate" in body["spec"]

        # Verify taskTemplate structure
        task_template = body["spec"]["taskTemplate"]
        assert "spec" in task_template
        assert "process" in task_template["spec"]
        command = task_template["spec"]["process"]["command"]
        assert command[0] == "/bin/sh"
        assert command[1] == "-c"
        # Command should contain bootstrap.sh execution
        # Example: /opt/opensandbox/bootstrap.sh python app.py &
        assert "/opt/opensandbox/bootstrap.sh python app.py" in command[2]
        assert command[2].endswith(" &")
        assert task_template["spec"]["process"]["env"] == [{"name": "FOO", "value": "bar"}]

    def test_build_task_template_with_env(self, mock_k8s_client):
        """
        Test _build_task_template with environment variables.

        Verifies:
        - Command uses shell wrapper: /bin/sh -c "..."
        - Entrypoint executed via bootstrap.sh in background (&)
        - Env list formatted correctly for K8s

        Generated command example:
        /bin/sh -c "/opt/opensandbox/bootstrap.sh /usr/bin/python app.py &"
        """
        provider = BatchSandboxProvider(mock_k8s_client)

        result = provider._build_task_template(
            entrypoint=["/usr/bin/python", "app.py"], env={"KEY1": "value1", "KEY2": "value2"}
        )

        assert "spec" in result
        assert "process" in result["spec"]
        process_task = result["spec"]["process"]

        # Verify command structure
        command = process_task["command"]
        assert command[0] == "/bin/sh"
        assert command[1] == "-c"
        # Should execute via bootstrap.sh in background (&)
        assert "/opt/opensandbox/bootstrap.sh" in command[2]
        assert "/usr/bin/python" in command[2]
        assert "app.py" in command[2]
        # Should end with & (run in background)
        assert command[2].endswith("&")

        # Verify env list
        assert process_task["env"] == [
            {"name": "KEY1", "value": "value1"},
            {"name": "KEY2", "value": "value2"},
        ]

    def test_build_task_template_without_env(self, mock_k8s_client):
        """
        Test _build_task_template without environment variables.

        Verifies command is wrapped in shell and executes via bootstrap.sh in background.

        Generated command example:
        /bin/sh -c "/opt/opensandbox/bootstrap.sh /usr/bin/python app.py &"
        """
        provider = BatchSandboxProvider(mock_k8s_client)

        result = provider._build_task_template(entrypoint=["/usr/bin/python", "app.py"], env={})

        assert "spec" in result
        assert "process" in result["spec"]
        process_task = result["spec"]["process"]
        assert process_task["env"] == []
        # Without env, command directly calls bootstrap.sh in background
        command = process_task["command"]
        assert command[0] == "/bin/sh"
        assert command[1] == "-c"
        # Check escaped entrypoint
        assert "/opt/opensandbox/bootstrap.sh" in command[2]
        assert "/usr/bin/python" in command[2]
        assert "app.py" in command[2]
        assert command[2].endswith(" &")

    def test_build_task_template_uses_default_env_path(self, mock_k8s_client):
        """
        Test that taskTemplate executes bootstrap.sh properly.

        Verifies:
        - Entrypoint is properly escaped
        - Command runs in background
        """
        provider = BatchSandboxProvider(mock_k8s_client)

        result = provider._build_task_template(
            entrypoint=["python", "app.py"], env={"TEST_VAR": "test_value"}
        )

        command = result["spec"]["process"]["command"][2]
        # Should execute bootstrap.sh in background
        assert "/opt/opensandbox/bootstrap.sh" in command
        assert "python" in command
        assert "app.py" in command
        assert command.endswith(" &")

    def test_build_task_template_escapes_special_characters(self, mock_k8s_client):
        """
        Test that taskTemplate properly escapes arguments with spaces, quotes, and special chars.

        This prevents shell injection and ensures arguments are preserved correctly.
        For example: ['python', '-c', 'print("a b")'] should work correctly.
        """
        provider = BatchSandboxProvider(mock_k8s_client)

        result = provider._build_task_template(
            entrypoint=["python", "-c", 'print("hello world")'],
            env={"KEY": "value with spaces", "QUOTE": "it's fine"},
        )

        command = result["spec"]["process"]["command"][2]

        # Verify entrypoint args are properly escaped
        assert "python" in command
        assert "-c" in command
        # The python code with spaces and quotes should be properly escaped
        assert "'print(" in command or '"print(' in command  # Escaped

        # Verify env is passed through env list, not in command
        env_list = result["spec"]["process"]["env"]
        assert {"name": "KEY", "value": "value with spaces"} in env_list
        assert {"name": "QUOTE", "value": "it's fine"} in env_list

    def test_create_workload_poolref_builds_correct_manifest(self, mock_k8s_client):
        """
        Test complete pool-based BatchSandbox manifest structure.

        Verifies:
        - Basic metadata (apiVersion, kind, name, labels)
        - Pool-specific fields (poolRef, taskTemplate, expireTime)
        - No template field (pool mode doesn't use pod template)
        """
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        expires_at = datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri=""),
            entrypoint=["python", "app.py"],
            env={"FOO": "bar"},
            resource_limits={},
            labels={"test": "label"},
            expires_at=expires_at,
            execd_image="execd:latest",
            extensions={"poolRef": "test-pool"},
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]

        # Verify basic structure
        assert body["apiVersion"] == "sandbox.opensandbox.io/v1alpha1"
        assert body["kind"] == "BatchSandbox"
        assert body["metadata"]["name"] == "test-id"
        assert body["metadata"]["labels"] == {"test": "label"}

        # Verify pool-specific fields
        assert body["spec"]["replicas"] == 1
        assert body["spec"]["poolRef"] == "test-pool"
        assert body["spec"]["expireTime"] == "2025-12-31T10:00:00+00:00"
        assert "taskTemplate" in body["spec"]

        # Verify no template field (pool-based doesn't use template)
        assert "template" not in body["spec"]

    def test_create_workload_poolref_default_entrypoint_no_env_omits_task_template(
        self, mock_k8s_client
    ):
        """When entrypoint is SDK default and env is empty, taskTemplate is omitted."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="dockurr/windows:latest"),
            entrypoint=["tail", "-f", "/dev/null"],
            env={},
            resource_limits={},
            labels={},
            expires_at=None,
            execd_image="execd:latest",
            extensions={"poolRef": "my-pool"},
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        assert body["spec"]["poolRef"] == "my-pool"
        assert "taskTemplate" not in body["spec"]

    def test_create_workload_poolref_default_entrypoint_with_env_includes_task_template(
        self, mock_k8s_client
    ):
        """When entrypoint is SDK default but env is non-empty, taskTemplate is generated."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="dockurr/windows:latest"),
            entrypoint=["tail", "-f", "/dev/null"],
            env={"VERSION": "11"},
            resource_limits={},
            labels={},
            expires_at=None,
            execd_image="execd:latest",
            extensions={"poolRef": "my-pool"},
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        assert body["spec"]["poolRef"] == "my-pool"
        assert "taskTemplate" in body["spec"]
        task_template = body["spec"]["taskTemplate"]
        assert task_template["spec"]["process"]["env"] == [{"name": "VERSION", "value": "11"}]

    def test_create_workload_poolref_none_entrypoint_no_env_omits_task_template(self, mock_k8s_client):
        """When entrypoint is None and env is empty, taskTemplate is omitted.

        SDK pool mode callers omit entrypoint entirely (None), expecting the pool's
        default command to run. This must not raise a TypeError.
        """
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=None,
            env={},
            resource_limits={},
            labels={},
            expires_at=None,
            execd_image="execd:latest",
            extensions={"poolRef": "my-pool"},
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        assert body["spec"]["poolRef"] == "my-pool"
        assert "taskTemplate" not in body["spec"]


class TestBatchSandboxProviderEgress:
    """BatchSandboxProvider egress sidecar tests"""

    def test_create_workload_without_network_policy_no_sidecar(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        expires_at = datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=expires_at,
            execd_image="execd:latest",
            network_policy=None,
            egress_image=None,
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]
        containers = pod_spec["containers"]

        # Should only have main container
        assert len(containers) == 1
        assert containers[0]["name"] == "sandbox"
        # Should not have securityContext with sysctls
        assert "securityContext" not in pod_spec or "sysctls" not in pod_spec.get(
            "securityContext", {}
        )

    def test_create_workload_with_network_policy_adds_sidecar(self, mock_k8s_client):
        provider = BatchSandboxProvider(
            mock_k8s_client,
            _app_config_with_egress_disable_ipv6(),
        )
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        expires_at = datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="pypi.org")],
        )

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=expires_at,
            execd_image="execd:latest",
            network_policy=network_policy,
            egress_image="opensandbox/egress:v1.1.3",
            credential_proxy_enabled=True,
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]
        containers = pod_spec["containers"]

        # Should have both main container and sidecar
        assert len(containers) == 2

        # Find sidecar container
        sidecar = next((c for c in containers if c["name"] == "egress"), None)
        assert sidecar is not None
        assert sidecar["image"] == "opensandbox/egress:v1.1.3"

        # Verify sidecar has environment variable
        env_vars = {e["name"]: e["value"] for e in sidecar.get("env", [])}
        assert "OPENSANDBOX_EGRESS_RULES" in env_vars
        assert env_vars["OPENSANDBOX_EGRESS_MODE"] == EGRESS_MODE_DNS
        assert env_vars[OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT] == "true"

        caps = sidecar.get("securityContext", {}).get("capabilities", {})
        assert "NET_ADMIN" in caps.get("add", [])
        assert sidecar.get("securityContext", {}).get("privileged") is not True
        assert "command" not in sidecar
        assert sidecar["readinessProbe"]["httpGet"]["path"] == "/healthz"
        assert sidecar["readinessProbe"]["httpGet"]["port"] == 18080

        inits = pod_spec.get("initContainers", [])
        assert len(inits) == 1
        execd_init = inits[0]
        assert execd_init["name"] == "execd-installer"
        assert execd_init["image"] == "execd:latest"
        assert execd_init.get("securityContext", {}).get("privileged") is True
        assert "/proc/sys/net/ipv6/conf/all/disable_ipv6" in execd_init["args"][0]

        main = next(c for c in containers if c["name"] == "sandbox")
        main_env = {e["name"]: e["value"] for e in main["env"]}
        assert main_env[OPENSANDBOX_EGRESS_MITMPROXY_TRANSPARENT] == "true"
        assert "SSL_CERT_FILE" not in main_env
        assert "REQUESTS_CA_BUNDLE" not in main_env
        assert "CURL_CA_BUNDLE" not in main_env
        assert "GIT_SSL_CAINFO" not in main_env
        assert "NODE_EXTRA_CA_CERTS" not in main_env
        assert "opensandbox-mitm-ca" not in {v["name"] for v in pod_spec["volumes"]}
        assert {
            "name": OPENSANDBOX_RUNTIME_VOLUME_NAME,
            "mountPath": OPENSANDBOX_RUNTIME_MOUNT_PATH,
        } in main["volumeMounts"]
        assert {
            "name": OPENSANDBOX_RUNTIME_VOLUME_NAME,
            "mountPath": OPENSANDBOX_RUNTIME_MOUNT_PATH,
        } in sidecar["volumeMounts"]

    def test_create_workload_windows_profile_with_network_policy_keeps_ipv6_disable(
        self, mock_k8s_client
    ):
        provider = BatchSandboxProvider(
            mock_k8s_client,
            _app_config_with_egress_disable_ipv6(),
        )
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="dockurr/windows:latest"),
            entrypoint=["cmd", "/c", "echo hello"],
            env={"VERSION": "11"},
            resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
            labels={},
            expires_at=None,
            execd_image="execd:latest",
            platform=PlatformSpec(os="windows", arch="amd64"),
            network_policy=NetworkPolicy(default_action="deny", egress=[]),
            egress_image="opensandbox/egress:v1.1.3",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]

        sidecar = next((c for c in pod_spec["containers"] if c["name"] == "egress"), None)
        assert sidecar is not None

        main_container = next((c for c in pod_spec["containers"] if c["name"] == "sandbox"), None)
        assert main_container is not None
        main_caps = main_container.get("securityContext", {}).get("capabilities", {})
        assert "NET_ADMIN" in main_caps.get("add", [])
        assert "NET_RAW" in main_caps.get("add", [])
        assert "NET_ADMIN" not in main_caps.get("drop", [])

        execd_init = pod_spec["initContainers"][0]
        assert execd_init.get("securityContext", {}).get("privileged") is True
        assert "/proc/sys/net/ipv6/conf/all/disable_ipv6" in execd_init["args"][0]

    def test_create_workload_with_network_policy_persists_annotation_and_sidecar_token(
        self, mock_k8s_client
    ):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=None,
            execd_image="execd:latest",
            network_policy=NetworkPolicy(default_action="deny", egress=[]),
            egress_image="opensandbox/egress:v1.1.3",
            annotations={SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY: "egress-token"},
            egress_auth_token="egress-token",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        assert (
            body["metadata"]["annotations"][SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY]
            == "egress-token"
        )

        containers = body["spec"]["template"]["spec"]["containers"]
        sidecar = next((c for c in containers if c["name"] == "egress"), None)
        assert sidecar is not None
        env_vars = {e["name"]: e["value"] for e in sidecar.get("env", [])}
        assert env_vars[OPENSANDBOX_EGRESS_TOKEN] == "egress-token"
        assert env_vars["OPENSANDBOX_EGRESS_MODE"] == EGRESS_MODE_DNS
        assert sidecar["readinessProbe"]["httpGet"]["httpHeaders"] == [
            {"name": OPEN_SANDBOX_EGRESS_AUTH_HEADER, "value": "egress-token"}
        ]

    def test_create_workload_with_egress_mode_dns_nft(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=None,
            execd_image="execd:latest",
            network_policy=NetworkPolicy(default_action="deny", egress=[]),
            egress_image="opensandbox/egress:v1.1.3",
            egress_mode=EGRESS_MODE_DNS_NFT,
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        containers = body["spec"]["template"]["spec"]["containers"]
        sidecar = next((c for c in containers if c["name"] == "egress"), None)
        assert sidecar is not None
        env_vars = {e["name"]: e["value"] for e in sidecar.get("env", [])}
        assert env_vars["OPENSANDBOX_EGRESS_MODE"] == EGRESS_MODE_DNS_NFT

    def test_create_workload_with_network_policy_does_not_add_pod_ipv6_sysctls(
        self, mock_k8s_client
    ):
        """IPv6 all.disable is applied in privileged execd init, not Pod sysctls."""
        provider = BatchSandboxProvider(
            mock_k8s_client,
            _app_config_with_egress_disable_ipv6(),
        )
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        expires_at = datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=expires_at,
            execd_image="execd:latest",
            network_policy=network_policy,
            egress_image="opensandbox/egress:v1.1.3",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]

        assert "securityContext" not in pod_spec or "sysctls" not in pod_spec.get(
            "securityContext", {}
        )

        sidecar = next(c for c in pod_spec["containers"] if c["name"] == "egress")
        assert "command" not in sidecar
        execd_init = pod_spec["initContainers"][0]
        assert execd_init["name"] == "execd-installer"
        assert "/proc/sys/net/ipv6/conf/all/disable_ipv6" in execd_init["args"][0]

    def test_create_workload_with_egress_skips_ipv6_disable_when_not_configured(
        self, mock_k8s_client
    ):
        """With ``egress.disable_ipv6`` false, execd init is not privileged and does not write disable_ipv6."""
        provider = BatchSandboxProvider(
            mock_k8s_client,
            _app_config_with_egress_disable_ipv6(False),
        )
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=None,
            execd_image="execd:latest",
            network_policy=network_policy,
            egress_image="opensandbox/egress:v1.1.3",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]
        execd_init = pod_spec["initContainers"][0]
        assert execd_init["name"] == "execd-installer"
        assert "securityContext" not in execd_init
        assert "/proc/sys/net/ipv6/conf/all/disable_ipv6" not in execd_init["args"][0]

    def test_create_workload_with_network_policy_drops_net_admin_from_main_container(
        self, mock_k8s_client
    ):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        expires_at = datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=expires_at,
            execd_image="execd:latest",
            network_policy=network_policy,
            egress_image="opensandbox/egress:v1.1.3",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]
        containers = pod_spec["containers"]

        # Find main container
        main_container = next((c for c in containers if c["name"] == "sandbox"), None)
        assert main_container is not None

        # Verify main container has securityContext
        assert "securityContext" in main_container
        assert "capabilities" in main_container["securityContext"]
        assert "drop" in main_container["securityContext"]["capabilities"]
        assert "NET_ADMIN" in main_container["securityContext"]["capabilities"]["drop"]

    def test_create_workload_without_egress_image_no_sidecar(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        expires_at = datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=expires_at,
            execd_image="execd:latest",
            network_policy=network_policy,
            egress_image=None,
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]
        containers = pod_spec["containers"]

        # Should only have main container
        assert len(containers) == 1
        assert containers[0]["name"] == "sandbox"

    def test_egress_sidecar_contains_network_policy_in_env(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        expires_at = datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[
                NetworkRule(action="allow", target="pypi.org"),
                NetworkRule(action="deny", target="*.malicious.com"),
            ],
        )

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=expires_at,
            execd_image="execd:latest",
            network_policy=network_policy,
            egress_image="opensandbox/egress:v1.1.3",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]
        containers = pod_spec["containers"]

        sidecar = next((c for c in containers if c["name"] == "egress"), None)
        assert sidecar is not None

        env_vars = {e["name"]: e["value"] for e in sidecar.get("env", [])}
        assert "OPENSANDBOX_EGRESS_RULES" in env_vars

        # Verify the environment variable contains valid JSON with network policy
        import json

        policy_json = json.loads(env_vars["OPENSANDBOX_EGRESS_RULES"])
        assert policy_json["defaultAction"] == "deny"
        assert len(policy_json["egress"]) == 2
        assert policy_json["egress"][0]["action"] == "allow"
        assert policy_json["egress"][0]["target"] == "pypi.org"

    def test_main_container_no_security_context_without_network_policy(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        expires_at = datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=expires_at,
            execd_image="execd:latest",
            network_policy=None,
            egress_image=None,
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]
        containers = pod_spec["containers"]

        main_container = containers[0]
        # Main container should not have securityContext when no network policy
        assert "securityContext" not in main_container

    def test_create_workload_with_network_policy_works_with_template(
        self, mock_k8s_client, tmp_path
    ):
        template_file = tmp_path / "template.yaml"
        template_file.write_text(
            """
spec:
  template:
    spec:
      volumes:
        - name: sandbox-shared-data
          emptyDir: {}
"""
        )
        provider = BatchSandboxProvider(
            mock_k8s_client, _app_config_with_template(str(template_file))
        )
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        expires_at = datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)
        network_policy = NetworkPolicy(
            default_action="deny",
            egress=[NetworkRule(action="allow", target="example.com")],
        )

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=expires_at,
            execd_image="execd:latest",
            network_policy=network_policy,
            egress_image="opensandbox/egress:v1.1.3",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]
        containers = pod_spec["containers"]

        # Should have both main container and sidecar
        assert len(containers) == 2

        # Verify sidecar exists
        sidecar = next((c for c in containers if c["name"] == "egress"), None)
        assert sidecar is not None

        # Pod-level IPv6 sysctls are not injected for egress (sidecar startup handles all.disable)
        assert "securityContext" not in pod_spec or "sysctls" not in pod_spec.get(
            "securityContext", {}
        )

        # Verify template volumes are still merged
        volume_names = [v["name"] for v in pod_spec["volumes"]]
        assert "sandbox-shared-data" in volume_names
        assert "opensandbox-bin" in volume_names

    # ===== Phase + Condition Validation Tests =====

    def test_pause_sandbox_running_allows(self, mock_k8s_client):
        """Test pause allowed when Phase=Succeed."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {"phase": "Succeed", "conditions": []},
        }
        mock_k8s_client.patch_custom_object.return_value = {}

        provider.pause_sandbox("test-id", "test-ns")

        mock_k8s_client.patch_custom_object.assert_called_once()
        call_kwargs = mock_k8s_client.patch_custom_object.call_args.kwargs
        assert call_kwargs["body"] == {"spec": {"pause": True}}

    def test_pause_sandbox_running_with_pause_failed_allows_retry(self, mock_k8s_client):
        """Test pause retry performs an internal nil->true double patch."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {
                "phase": "Succeed",
                "conditions": [
                    {"type": "PauseFailed", "status": "True", "reason": "SnapshotFailed"}
                ],
            },
        }
        mock_k8s_client.patch_custom_object.return_value = {}

        provider.pause_sandbox("test-id", "test-ns")

        assert mock_k8s_client.patch_custom_object.call_count == 2
        first_patch = mock_k8s_client.patch_custom_object.call_args_list[0].kwargs["body"]
        second_patch = mock_k8s_client.patch_custom_object.call_args_list[1].kwargs["body"]
        assert first_patch == {"spec": {"pause": None}}
        assert second_patch == {"spec": {"pause": True}}

    def test_patch_pause_with_retry_bridge_accepts_second_patch_timeout_when_readback_matches_target(
        self, mock_k8s_client
    ):
        provider = BatchSandboxProvider(mock_k8s_client)
        provider.patch_workload = MagicMock(
            side_effect=[{}, ApiException(status=500, reason="timeout")]
        )
        provider.get_workload = MagicMock(
            return_value={
                "metadata": {"name": "test-id", "namespace": "test-ns"},
                "spec": {"pause": True},
            }
        )

        provider._patch_pause_with_retry_bridge("test-id", "test-ns", True)

        assert provider.patch_workload.call_count == 2
        first_call = provider.patch_workload.call_args_list[0].args
        second_call = provider.patch_workload.call_args_list[1].args
        assert first_call == ("test-id", "test-ns", {"spec": {"pause": None}})
        assert second_call == ("test-id", "test-ns", {"spec": {"pause": True}})
        provider.get_workload.assert_called_once_with("test-id", "test-ns")

    def test_patch_pause_with_retry_bridge_retries_target_when_readback_still_nil(
        self, mock_k8s_client
    ):
        provider = BatchSandboxProvider(mock_k8s_client)
        provider.patch_workload = MagicMock(
            side_effect=[{}, ApiException(status=500, reason="timeout"), {}]
        )
        provider.get_workload = MagicMock(
            return_value={
                "metadata": {"name": "test-id", "namespace": "test-ns"},
                "spec": {"pause": None},
            }
        )

        provider._patch_pause_with_retry_bridge("test-id", "test-ns", True)

        assert provider.patch_workload.call_count == 3
        first_call = provider.patch_workload.call_args_list[0].args
        second_call = provider.patch_workload.call_args_list[1].args
        third_call = provider.patch_workload.call_args_list[2].args
        assert first_call == ("test-id", "test-ns", {"spec": {"pause": None}})
        assert second_call == ("test-id", "test-ns", {"spec": {"pause": True}})
        assert third_call == ("test-id", "test-ns", {"spec": {"pause": True}})
        provider.get_workload.assert_called_once_with("test-id", "test-ns")

    def test_pause_sandbox_pausing_rejects(self, mock_k8s_client):
        """Test pause rejected when Phase=Pausing."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {"phase": "Pausing", "conditions": []},
        }

        with pytest.raises(ValueError, match="operation in progress"):
            provider.pause_sandbox("test-id", "test-ns")

    def test_pause_sandbox_resuming_rejects(self, mock_k8s_client):
        """Test pause rejected when Phase=Resuming."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {"phase": "Resuming", "conditions": []},
        }

        with pytest.raises(ValueError, match="operation in progress"):
            provider.pause_sandbox("test-id", "test-ns")

    def test_pause_sandbox_paused_rejects(self, mock_k8s_client):
        """Test pause rejected when Phase=Paused."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {"phase": "Paused", "conditions": []},
        }

        with pytest.raises(ValueError, match="already paused"):
            provider.pause_sandbox("test-id", "test-ns")

    def test_pause_sandbox_failed_rejects(self, mock_k8s_client):
        """Test pause rejected when Phase=Failed."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {"phase": "Failed", "conditions": []},
        }

        with pytest.raises(ValueError, match="not available"):
            provider.pause_sandbox("test-id", "test-ns")

    def test_pause_sandbox_failed_with_pause_failed_rejects(self, mock_k8s_client):
        """Test pause rejected when Phase=Failed + PauseFailed=True (pod loss scenario)."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {
                "phase": "Failed",
                "conditions": [{"type": "PauseFailed", "status": "True", "reason": "PodNotFound"}],
            },
        }

        with pytest.raises(ValueError, match="pause caused pod loss"):
            provider.pause_sandbox("test-id", "test-ns")

    def test_pause_sandbox_pending_rejects(self, mock_k8s_client):
        """Test pause rejected when Phase=Pending."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {"phase": "Pending", "conditions": []},
        }

        with pytest.raises(ValueError, match="being created"):
            provider.pause_sandbox("test-id", "test-ns")

    def test_resume_sandbox_paused_allows(self, mock_k8s_client):
        """Test resume allowed when Phase=Paused."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {"phase": "Paused", "conditions": []},
        }
        mock_k8s_client.patch_custom_object.return_value = {}

        provider.resume_sandbox("test-id", "test-ns")

        mock_k8s_client.patch_custom_object.assert_called_once()
        call_kwargs = mock_k8s_client.patch_custom_object.call_args.kwargs
        assert call_kwargs["body"] == {"spec": {"pause": False}}

    def test_resume_sandbox_paused_with_resume_failed_allows_retry(self, mock_k8s_client):
        """Test resume retry performs an internal nil->false double patch."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {
                "phase": "Paused",
                "conditions": [
                    {"type": "ResumeFailed", "status": "True", "reason": "SnapshotNotReady"}
                ],
            },
        }
        mock_k8s_client.patch_custom_object.return_value = {}

        provider.resume_sandbox("test-id", "test-ns")

        assert mock_k8s_client.patch_custom_object.call_count == 2
        first_patch = mock_k8s_client.patch_custom_object.call_args_list[0].kwargs["body"]
        second_patch = mock_k8s_client.patch_custom_object.call_args_list[1].kwargs["body"]
        assert first_patch == {"spec": {"pause": None}}
        assert second_patch == {"spec": {"pause": False}}

    def test_resume_sandbox_resuming_rejects(self, mock_k8s_client):
        """Test resume rejected when Phase=Resuming."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {"phase": "Resuming", "conditions": []},
        }

        with pytest.raises(ValueError, match="operation in progress"):
            provider.resume_sandbox("test-id", "test-ns")

    def test_resume_sandbox_pausing_rejects(self, mock_k8s_client):
        """Test resume rejected when Phase=Pausing."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {"phase": "Pausing", "conditions": []},
        }

        with pytest.raises(ValueError, match="operation in progress"):
            provider.resume_sandbox("test-id", "test-ns")

    def test_resume_sandbox_running_rejects(self, mock_k8s_client):
        """Test resume rejected when Phase=Succeed."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {"phase": "Succeed", "conditions": []},
        }

        with pytest.raises(ValueError, match="expected Paused"):
            provider.resume_sandbox("test-id", "test-ns")

    def test_get_status_succeed_phase_maps_to_running_state(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {
            "status": {"phase": "Succeed"},
            "metadata": {"creationTimestamp": "2025-12-24T10:00:00Z"},
        }

        result = provider.get_status(workload)

        assert result["state"] == "Running"
        assert result["reason"] == "RUNNING"
        assert result["message"] == "Sandbox is running"

    def test_get_status_failed_uses_condition_message(self):
        provider = BatchSandboxProvider(MagicMock())
        workload = {
            "status": {
                "phase": "Failed",
                "conditions": [
                    {
                        "type": "ResumeFailed",
                        "status": "True",
                        "reason": "PodStartFailed",
                        "message": "Pod sandbox-abc-0: ImagePullBackOff - image not found",
                    }
                ],
            },
            "metadata": {"creationTimestamp": "2025-12-24T10:00:00Z"},
        }

        result = provider.get_status(workload)

        assert result["state"] == "Failed"
        assert result["reason"] == "FAILED"
        assert result["message"] == "Pod sandbox-abc-0: ImagePullBackOff - image not found"

    def test_resume_sandbox_failed_rejects(self, mock_k8s_client):
        """Test resume rejected when Phase=Failed."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {"phase": "Failed", "conditions": []},
        }

        with pytest.raises(ValueError, match="not available"):
            provider.resume_sandbox("test-id", "test-ns")

    def test_resume_sandbox_failed_with_resume_failed_rejects(self, mock_k8s_client):
        """Test resume rejected when Phase=Failed + ResumeFailed=True (pod start failure)."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {
                "phase": "Failed",
                "conditions": [
                    {"type": "ResumeFailed", "status": "True", "reason": "PodStartFailed"}
                ],
            },
        }

        with pytest.raises(ValueError, match="resume caused pod start failure"):
            provider.resume_sandbox("test-id", "test-ns")

    def test_resume_sandbox_pending_rejects(self, mock_k8s_client):
        """Test resume rejected when Phase=Pending."""
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {
            "metadata": {"name": "test-id", "namespace": "test-ns"},
            "status": {"phase": "Pending", "conditions": []},
        }

        with pytest.raises(ValueError, match="being created"):
            provider.resume_sandbox("test-id", "test-ns")

    # ===== Image Auth Tests =====

    def test_supports_image_auth_returns_true(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        assert provider.supports_image_auth() is True

    def test_create_workload_with_image_auth_injects_image_pull_secrets(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "uid-123"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(
                uri="registry.example.com/img:tag",
                auth=ImageAuth(username="user", password="pass"),
            ),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pull_secrets = body["spec"]["template"]["spec"].get("imagePullSecrets")
        assert pull_secrets == [{"name": f"{IMAGE_AUTH_SECRET_PREFIX}-test-id"}]

    def test_create_workload_with_image_auth_creates_secret(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "uid-abc"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(
                uri="registry.example.com/img:tag",
                auth=ImageAuth(username="user", password="pass"),
            ),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        mock_k8s_client.create_secret.assert_called_once()
        call_kwargs = mock_k8s_client.create_secret.call_args.kwargs
        assert call_kwargs["namespace"] == "test-ns"
        secret = call_kwargs["body"]
        assert secret.type == "kubernetes.io/dockerconfigjson"
        ref = secret.metadata.owner_references[0]
        assert ref.uid == "uid-abc"
        assert ref.kind == "BatchSandbox"
        assert ref.name == "test-id"

    def test_create_workload_without_image_auth_skips_secret(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "uid-123"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        mock_k8s_client.create_secret.assert_not_called()
        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        assert "imagePullSecrets" not in body["spec"]["template"]["spec"]

    def test_create_workload_with_image_auth_secret_failure_rolls_back_batchsandbox(
        self, mock_k8s_client
    ):
        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "uid-123"}
        }
        mock_k8s_client.create_secret.side_effect = ApiException(status=403)

        with pytest.raises(ApiException):
            provider.create_workload(
                sandbox_id="test-id",
                namespace="test-ns",
                image_spec=ImageSpec(
                    uri="registry.example.com/img:tag",
                    auth=ImageAuth(username="user", password="pass"),
                ),
                entrypoint=["/bin/bash"],
                env={},
                resource_limits={},
                labels={},
                expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
                execd_image="execd:latest",
            )

        mock_k8s_client.delete_custom_object.assert_called_once_with(
            group=provider.group,
            version=provider.version,
            namespace="test-ns",
            plural=provider.plural,
            name="test-id",
            grace_period_seconds=0,
        )

    # ===== Volume Support Tests =====

    def test_create_workload_with_pvc_volume(self, mock_k8s_client):
        """
        Test creating workload with PVC volume mount.

        Verifies:
        - PVC volume is correctly added to pod spec
        - Volume mount is added to main container
        - claimName is correctly set
        """
        from opensandbox_server.api.schema import Volume, PVC

        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        expires_at = datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)

        volumes = [
            Volume(
                name="data-volume",
                pvc=PVC(claim_name="my-pvc"),
                mount_path="/mnt/data",
                read_only=False,
            )
        ]

        result = provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=expires_at,
            execd_image="execd:latest",
            volumes=volumes,
        )

        assert result == {"name": "test-id", "uid": "test-uid", "apiVersion": "sandbox.opensandbox.io/v1alpha1", "kind": "BatchSandbox"}

    def test_create_workload_poolref_rejects_platform(self, mock_k8s_client):
        provider = BatchSandboxProvider(mock_k8s_client)

        with pytest.raises(
            ValueError, match="platform is not supported together with extensions.poolRef"
        ):
            provider.create_workload(
                sandbox_id="test-id",
                namespace="test-ns",
                image_spec=ImageSpec(uri="python:3.11"),
                entrypoint=["/bin/bash"],
                env={},
                resource_limits={},
                labels={"opensandbox.io/id": "test-id"},
                expires_at=None,
                execd_image="execd:latest",
                extensions={"poolRef": "warm-pool"},
                platform=PlatformSpec(os="linux", arch="amd64"),
            )

    def test_create_workload_with_pvc_volume_readonly(self, mock_k8s_client):
        """
        Test creating workload with read-only PVC volume mount.
        """
        from opensandbox_server.api.schema import Volume, PVC

        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        volumes = [
            Volume(
                name="models-volume",
                pvc=PVC(claim_name="models-pvc"),
                mount_path="/mnt/models",
                read_only=True,
            )
        ]

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
            volumes=volumes,
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]

        main_container = pod_spec["containers"][0]
        mounts = main_container.get("volumeMounts", [])
        models_mount = next((m for m in mounts if m["name"] == "models-volume"), None)
        assert models_mount is not None
        assert models_mount["readOnly"] is True

    def test_create_workload_with_pvc_volume_subpath(self, mock_k8s_client):
        """
        Test creating workload with PVC volume mount with subPath.
        """
        from opensandbox_server.api.schema import Volume, PVC

        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        volumes = [
            Volume(
                name="data-volume",
                pvc=PVC(claim_name="shared-pvc"),
                mount_path="/mnt/data",
                sub_path="task-001",
                read_only=False,
            )
        ]

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
            volumes=volumes,
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]

        main_container = pod_spec["containers"][0]
        mounts = main_container.get("volumeMounts", [])
        data_mount = next((m for m in mounts if m["name"] == "data-volume"), None)
        assert data_mount is not None
        assert data_mount.get("subPath") == "task-001"

    def test_create_workload_with_host_volume(self, mock_k8s_client):
        """
        Test creating workload with hostPath volume mount.
        """
        from opensandbox_server.api.schema import Volume, Host

        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        volumes = [
            Volume(
                name="host-volume",
                host=Host(path="/data/shared"),
                mount_path="/mnt/host",
                read_only=True,
            )
        ]

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
            volumes=volumes,
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]

        # Check volume definition
        volumes_list = pod_spec.get("volumes", [])
        host_volume = next((v for v in volumes_list if v["name"] == "host-volume"), None)
        assert host_volume is not None
        assert host_volume["hostPath"]["path"] == "/data/shared"
        assert host_volume["hostPath"]["type"] == "DirectoryOrCreate"

        # Check volume mount
        main_container = pod_spec["containers"][0]
        mounts = main_container.get("volumeMounts", [])
        host_mount = next((m for m in mounts if m["name"] == "host-volume"), None)
        assert host_mount is not None
        assert host_mount["mountPath"] == "/mnt/host"
        assert host_mount["readOnly"] is True

    def test_create_workload_with_multiple_volumes(self, mock_k8s_client):
        """
        Test creating workload with multiple volumes (PVC and hostPath).
        """
        from opensandbox_server.api.schema import Volume, PVC, Host

        provider = BatchSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        volumes = [
            Volume(
                name="pvc-volume",
                pvc=PVC(claim_name="data-pvc"),
                mount_path="/mnt/data",
                read_only=False,
            ),
            Volume(
                name="host-volume",
                host=Host(path="/tmp/cache"),
                mount_path="/mnt/cache",
                read_only=True,
            ),
        ]

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={},
            labels={},
            expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
            execd_image="execd:latest",
            volumes=volumes,
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        pod_spec = body["spec"]["template"]["spec"]

        # Check both volumes exist
        volumes_list = pod_spec.get("volumes", [])
        assert len([v for v in volumes_list if v["name"] in ("pvc-volume", "host-volume")]) == 2

        # Check both mounts exist
        main_container = pod_spec["containers"][0]
        mounts = main_container.get("volumeMounts", [])
        mount_names = {m["name"] for m in mounts}
        assert "pvc-volume" in mount_names
        assert "host-volume" in mount_names

    def test_create_workload_pool_mode_rejects_volumes(self, mock_k8s_client):
        """
        Test that pool mode rejects volumes with clear error message.
        """
        from opensandbox_server.api.schema import Volume, PVC

        provider = BatchSandboxProvider(mock_k8s_client)

        volumes = [
            Volume(
                name="data-volume",
                pvc=PVC(claim_name="my-pvc"),
                mount_path="/mnt/data",
            )
        ]

        with pytest.raises(ValueError, match="Pool mode does not support volumes"):
            provider.create_workload(
                sandbox_id="test-id",
                namespace="test-ns",
                image_spec=ImageSpec(uri="python:3.11"),
                entrypoint=["/bin/bash"],
                env={},
                resource_limits={},
                labels={},
                expires_at=datetime(2025, 12, 31, tzinfo=timezone.utc),
                execd_image="execd:latest",
                extensions={"poolRef": "my-pool"},
                volumes=volumes,
            )

    def test_apply_volumes_to_pod_spec_empty_volumes(self, mock_k8s_client):
        """
        Test apply_volumes_to_pod_spec with empty volumes list.
        """
        pod_spec = {
            "containers": [{"name": "main", "volumeMounts": []}],
            "volumes": [],
        }

        apply_volumes_to_pod_spec(pod_spec, [])

        # Should not modify pod_spec
        assert pod_spec["volumes"] == []
        assert pod_spec["containers"][0]["volumeMounts"] == []

    def test_apply_volumes_to_pod_spec_no_containers(self, mock_k8s_client):
        """
        Test apply_volumes_to_pod_spec with no containers returns early without error.
        """
        from opensandbox_server.api.schema import Volume, PVC

        pod_spec = {"volumes": []}
        volumes = [Volume(name="test", pvc=PVC(claim_name="pvc"), mount_path="/mnt")]

        # Should not raise exception
        apply_volumes_to_pod_spec(pod_spec, volumes)

        # Pod spec should remain unchanged (no containers to mount to)
        assert pod_spec["volumes"] == []

    def test_apply_volumes_to_pod_spec_duplicate_internal_volume(self, mock_k8s_client):
        """
        Test apply_volumes_to_pod_spec rejects volume names that collide with internal volumes.
        """
        from opensandbox_server.api.schema import Volume, PVC

        pod_spec = {
            "containers": [{"name": "sandbox", "volumeMounts": []}],
            "volumes": [{"name": "opensandbox-bin", "emptyDir": {}}],
        }
        volumes = [Volume(name="opensandbox-bin", pvc=PVC(claim_name="pvc"), mount_path="/mnt")]

        # Should raise ValueError for duplicate volume name
        with pytest.raises(ValueError) as exc_info:
            apply_volumes_to_pod_spec(pod_spec, volumes)

        assert "conflicts with an internal volume" in str(exc_info.value)

    def test_apply_volumes_to_pod_spec_same_pvc_multiple_mounts(self, mock_k8s_client):
        """
        When multiple Volume API objects share the same claim_name, only one
        Kubernetes volume is created; multiple volumeMounts reference it (avoids
        CSI driver issues from duplicate PVC volume definitions).
        """
        from opensandbox_server.api.schema import Volume, PVC

        pod_spec = {
            "containers": [{"name": "main", "volumeMounts": []}],
            "volumes": [],
        }
        volumes = [
            Volume(
                name="skills",
                pvc=PVC(claim_name="oss-pvc-r"),
                mount_path="/path/to/skills",
                sub_path="skill-hub/publish",
                read_only=True,
            ),
            Volume(
                name="draft",
                pvc=PVC(claim_name="oss-pvc-r"),
                mount_path="/path/to/draft",
                sub_path="skill-hub/draft",
                read_only=True,
            ),
        ]

        apply_volumes_to_pod_spec(pod_spec, volumes)

        # One volume definition for the shared PVC (first Volume name used)
        assert len(pod_spec["volumes"]) == 1
        assert pod_spec["volumes"][0]["name"] == "skills"
        assert pod_spec["volumes"][0]["persistentVolumeClaim"]["claimName"] == "oss-pvc-r"

        # Two volumeMounts, both referencing the same volume name
        mounts = pod_spec["containers"][0]["volumeMounts"]
        assert len(mounts) == 2
        by_path = {m["mountPath"]: m for m in mounts}
        assert by_path["/path/to/skills"]["name"] == "skills"
        assert by_path["/path/to/skills"].get("subPath") == "skill-hub/publish"
        assert by_path["/path/to/draft"]["name"] == "skills"
        assert by_path["/path/to/draft"].get("subPath") == "skill-hub/draft"
