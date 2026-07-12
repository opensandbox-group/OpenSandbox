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

from datetime import datetime, timezone
from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest
from fastapi import HTTPException
from kubernetes.client import ApiException

from opensandbox_server.api.schema import ImageSpec, NetworkPolicy, NetworkRule, PlatformSpec
from opensandbox_server.config import (
    AppConfig,
    AgentSandboxRuntimeConfig,
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
from opensandbox_server.services.k8s.agent_sandbox_provider import AgentSandboxProvider
from opensandbox_server.services.constants import OPENSANDBOX_EGRESS_TOKEN


def _app_config(
    shutdown_policy: str = "Delete",
    execd_init_resources: ExecdInitResources | None = None,
    egress: EgressConfig | None = None,
) -> AppConfig:
    """Build an AppConfig for AgentSandboxProvider tests."""
    return AppConfig(
        runtime=RuntimeConfig(type="kubernetes", execd_image="execd:test"),
        kubernetes=KubernetesRuntimeConfig(
            namespace="test-ns",
            workload_provider="agent-sandbox",
            execd_init_resources=execd_init_resources,
        ),
        agent_sandbox=AgentSandboxRuntimeConfig(shutdown_policy=shutdown_policy),
        egress=egress,
    )


class TestAgentSandboxProvider:
    def test_init_sets_crd_constants_correctly(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)

        assert provider.group == "agents.x-k8s.io"
        assert provider.version == "v1alpha1"
        assert provider.plural == "sandboxes"

    def test_create_workload_builds_correct_manifest_init_mode(self, mock_k8s_client):
        provider = AgentSandboxProvider(
            mock_k8s_client,
            _app_config(shutdown_policy="Delete"),
        )
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

        assert result == {"name": "test-id", "uid": "test-uid", "apiVersion": "agents.x-k8s.io/v1alpha1", "kind": "Sandbox"}

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        assert body["apiVersion"] == "agents.x-k8s.io/v1alpha1"
        assert body["kind"] == "Sandbox"
        assert body["metadata"]["name"] == "test-id"
        assert body["metadata"]["namespace"] == "test-ns"
        assert body["spec"]["replicas"] == 1
        assert body["spec"]["shutdownTime"] == "2025-12-31T10:00:00+00:00"
        assert body["spec"]["shutdownPolicy"] == "Delete"
        assert body["spec"]["podTemplate"]["spec"]["automountServiceAccountToken"] is False
        assert "serviceAccountName" not in body["spec"]["podTemplate"]["spec"]
        assert "initContainers" in body["spec"]["podTemplate"]["spec"]
        assert "containers" in body["spec"]["podTemplate"]["spec"]
        assert "volumes" in body["spec"]["podTemplate"]["spec"]

    def test_create_workload_injects_platform_node_selector(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client, _app_config())
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
        selector = body["spec"]["podTemplate"]["spec"]["nodeSelector"]
        assert selector["kubernetes.io/os"] == "linux"
        assert selector["kubernetes.io/arch"] == "arm64"

    def test_create_workload_uses_separate_resource_requests(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client, _app_config())
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={"cpu": "2", "memory": "2Gi"},
            resource_requests={"cpu": "500m", "memory": "512Mi"},
            labels={"opensandbox.io/id": "test-id"},
            expires_at=None,
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        resources = body["spec"]["podTemplate"]["spec"]["containers"][0]["resources"]

        assert resources["limits"] == {"cpu": "2", "memory": "2Gi"}
        assert resources["requests"] == {"cpu": "500m", "memory": "512Mi"}

    def test_create_workload_translates_gpu_to_nvidia_extended_resource(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client, _app_config())
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "test-id", "uid": "test-uid"}
        }

        provider.create_workload(
            sandbox_id="test-id",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={},
            resource_limits={"cpu": "1", "memory": "1Gi", "gpu": "2"},
            labels={"opensandbox.io/id": "test-id"},
            expires_at=None,
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        resources = body["spec"]["podTemplate"]["spec"]["containers"][0]["resources"]

        assert resources["limits"]["nvidia.com/gpu"] == "2"
        assert resources["requests"]["nvidia.com/gpu"] == "2"
        assert "gpu" not in resources["limits"]
        assert "gpu" not in resources["requests"]

    def test_create_workload_without_gpu_omits_nvidia_extended_resource(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client, _app_config())
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
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        resources = body["spec"]["podTemplate"]["spec"]["containers"][0]["resources"]

        assert "nvidia.com/gpu" not in resources["limits"]
        assert "nvidia.com/gpu" not in resources["requests"]

    def test_create_workload_rejects_gpu_all_sentinel(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client, _app_config())

        with pytest.raises(HTTPException) as excinfo:
            provider.create_workload(
                sandbox_id="test-id",
                namespace="test-ns",
                image_spec=ImageSpec(uri="python:3.11"),
                entrypoint=["/bin/bash"],
                env={},
                resource_limits={"cpu": "1", "gpu": "all"},
                labels={"opensandbox.io/id": "test-id"},
                expires_at=None,
                execd_image="execd:latest",
            )
        assert excinfo.value.status_code == 400

    def test_create_workload_rejects_windows_platform(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client, _app_config())

        with pytest.raises(ValueError, match="agent-sandbox does not support platform.os=windows"):
            provider.create_workload(
                sandbox_id="test-id",
                namespace="test-ns",
                image_spec=ImageSpec(uri="dockurr/windows:latest"),
                entrypoint=["cmd", "/c", "echo hello"],
                env={},
                resource_limits={"cpu": "4", "memory": "8G", "disk": "64G"},
                labels={"opensandbox.io/id": "test-id"},
                expires_at=None,
                execd_image="execd:latest",
                platform=PlatformSpec(os="windows", arch="amd64"),
            )

    def test_create_workload_rejects_platform_conflict_with_template_selector(
        self, mock_k8s_client, tmp_path
    ):
        template_file = tmp_path / "agent_template.yaml"
        template_file.write_text(
            """
spec:
  podTemplate:
    spec:
      nodeSelector:
        kubernetes.io/os: linux
        kubernetes.io/arch: amd64
"""
        )
        app_config = _app_config()
        app_config.agent_sandbox.template_file = str(template_file)
        provider = AgentSandboxProvider(mock_k8s_client, app_config)

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
        template_file = tmp_path / "agent_template.yaml"
        template_file.write_text(
            """
spec:
  podTemplate:
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
        app_config = _app_config()
        app_config.agent_sandbox.template_file = str(template_file)
        provider = AgentSandboxProvider(mock_k8s_client, app_config)

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

    def test_create_workload_sanitizes_resource_name(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
        mock_k8s_client.create_custom_object.return_value = {
            "metadata": {"name": "sandbox-1234", "uid": "test-uid"}
        }

        expires_at = datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)

        result = provider.create_workload(
            sandbox_id="1234",
            namespace="test-ns",
            image_spec=ImageSpec(uri="python:3.11"),
            entrypoint=["/bin/bash"],
            env={"FOO": "bar"},
            resource_limits={"cpu": "1", "memory": "1Gi"},
            labels={"opensandbox.io/id": "1234"},
            expires_at=expires_at,
            execd_image="execd:latest",
        )

        assert result == {"name": "sandbox-1234", "uid": "test-uid", "apiVersion": "agents.x-k8s.io/v1alpha1", "kind": "Sandbox"}
        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        assert body["metadata"]["name"] == "sandbox-1234"

    def test_resource_name_uses_hash_when_id_has_no_alnum(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)

        first = provider._resource_name("!!!")
        second = provider._resource_name("???")

        assert first.startswith("sandbox-")
        assert second.startswith("sandbox-")
        assert first != second

    def test_get_workload_returns_none_on_404(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = None

        result = provider.get_workload("test-id", "test-ns")

        assert result is None

    def test_get_workload_prefers_sanitized_name(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.side_effect = [
            None,
            {"metadata": {"name": "1234"}},
        ]

        result = provider.get_workload("1234", "test-ns")

        assert result["metadata"]["name"] == "1234"
        assert mock_k8s_client.get_custom_object.call_args_list[0].kwargs["name"] == "sandbox-1234"
        assert mock_k8s_client.get_custom_object.call_args_list[1].kwargs["name"] == "1234"

    def test_get_workload_falls_back_to_legacy_name(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
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

    def test_get_workload_reraises_non_404_exceptions(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.side_effect = ApiException(status=500)

        with pytest.raises(ApiException) as exc_info:
            provider.get_workload("test-id", "test-ns")

        assert exc_info.value.status == 500

    def test_get_workload_prefers_informer_cache(self, mock_k8s_client):
        cached = {"metadata": {"name": "test-id"}}
        mock_k8s_client.get_custom_object.return_value = cached

        provider = AgentSandboxProvider(mock_k8s_client)

        result = provider.get_workload("test-id", "test-ns")

        assert result == cached
        mock_k8s_client.get_custom_object.assert_called()

    def test_create_workload_updates_informer_cache(self, mock_k8s_client):
        created_body = {"metadata": {"name": "test-id", "uid": "test-uid"}}
        mock_k8s_client.create_custom_object.return_value = created_body

        provider = AgentSandboxProvider(mock_k8s_client)

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

        assert result == {"name": "test-id", "uid": "test-uid", "apiVersion": "agents.x-k8s.io/v1alpha1", "kind": "Sandbox"}

    def test_update_expiration_patches_spec(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
        mock_k8s_client.get_custom_object.return_value = {"metadata": {"name": "sandbox-test-id"}}

        expires_at = datetime(2025, 12, 31, 0, 0, 0, tzinfo=timezone.utc)
        provider.update_expiration("test-id", "test-ns", expires_at)

        call_kwargs = mock_k8s_client.patch_custom_object.call_args.kwargs
        assert call_kwargs["body"] == {"spec": {"shutdownTime": "2025-12-31T00:00:00+00:00"}}

    def test_get_expiration_parses_z_suffix(self):
        provider = AgentSandboxProvider(MagicMock())
        workload = {"spec": {"shutdownTime": "2025-12-31T10:00:00Z"}}

        result = provider.get_expiration(workload)

        assert result == datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc)

    def test_get_status_ready_condition_true(self):
        provider = AgentSandboxProvider(MagicMock())
        workload = {
            "status": {
                "conditions": [
                    {
                        "type": "Ready",
                        "status": "True",
                        "reason": "SandboxReady",
                        "message": "Ready",
                        "lastTransitionTime": "2025-12-31T10:00:00Z",
                    }
                ]
            },
            "metadata": {"creationTimestamp": "2025-12-31T09:00:00Z"},
        }

        result = provider.get_status(workload)

        assert result["state"] == "Running"
        assert result["reason"] == "SandboxReady"
        assert result["message"] == "Ready"

    def test_get_status_expired_condition(self):
        provider = AgentSandboxProvider(MagicMock())
        workload = {
            "status": {
                "conditions": [
                    {
                        "type": "Ready",
                        "status": "False",
                        "reason": "SandboxExpired",
                        "message": "Expired",
                        "lastTransitionTime": "2025-12-31T10:00:00Z",
                    }
                ]
            },
            "metadata": {"creationTimestamp": "2025-12-31T09:00:00Z"},
        }

        result = provider.get_status(workload)

        assert result["state"] == "Terminated"
        assert result["reason"] == "SandboxExpired"

    def test_get_status_falls_back_to_pod_state(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
        mock_k8s_client.list_pods.return_value = [
            SimpleNamespace(status=SimpleNamespace(phase="Running", pod_ip="10.0.0.2"))
        ]
        workload = {
            "status": {"conditions": [], "selector": "app=sandbox"},
            "metadata": {"creationTimestamp": "2025-12-31T09:00:00Z", "namespace": "test-ns"},
        }

        result = provider.get_status(workload)

        assert result["state"] == "Running"
        assert result["reason"] == "POD_READY"

    def test_get_status_falls_back_to_allocated_when_ip_assigned_not_running(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
        mock_k8s_client.list_pods.return_value = [
            SimpleNamespace(status=SimpleNamespace(phase="Pending", pod_ip="10.0.0.2"))
        ]
        workload = {
            "status": {"conditions": [], "selector": "app=sandbox"},
            "metadata": {"creationTimestamp": "2025-12-31T09:00:00Z", "namespace": "test-ns"},
        }

        result = provider.get_status(workload)

        assert result["state"] == "Allocated"
        assert result["reason"] == "IP_ASSIGNED"

    def test_get_status_returns_failed_when_ready_condition_unschedulable(self):
        provider = AgentSandboxProvider(MagicMock())
        workload = {
            "spec": {
                "podTemplate": {
                    "spec": {
                        "nodeSelector": {
                            "kubernetes.io/os": "linux",
                            "kubernetes.io/arch": "arm64",
                        }
                    }
                }
            },
            "status": {
                "conditions": [
                    {
                        "type": "Ready",
                        "status": "False",
                        "reason": "FailedScheduling",
                        "message": "0/1 nodes are available: 1 node(s) didn't match Pod's node affinity.",
                        "lastTransitionTime": "2025-12-31T10:00:00Z",
                    }
                ]
            },
            "metadata": {"creationTimestamp": "2025-12-31T09:00:00Z"},
        }

        result = provider.get_status(workload)

        assert result["state"] == "Failed"
        assert result["reason"] == "POD_PLATFORM_UNSCHEDULABLE"
        assert "nodes are available" in result["message"]

    def test_get_status_keeps_pending_for_generic_scheduler_backpressure(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
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
            "status": {"conditions": [], "selector": "app=sandbox"},
            "metadata": {"creationTimestamp": "2025-12-31T09:00:00Z", "namespace": "test-ns"},
        }

        result = provider.get_status(workload)

        assert result["state"] == "Pending"
        assert result["reason"] in {"POD_SCHEDULED", "POD_PENDING"}

    def test_get_status_keeps_pending_when_non_platform_affinity_mismatch(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
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
                "podTemplate": {
                    "spec": {
                        "nodeSelector": {
                            "kubernetes.io/os": "linux",
                            "kubernetes.io/arch": "arm64",
                            "workload-class": "gpu",
                        }
                    }
                }
            },
            "status": {"conditions": [], "selector": "app=sandbox"},
            "metadata": {"creationTimestamp": "2025-12-31T09:00:00Z", "namespace": "test-ns"},
        }

        result = provider.get_status(workload)

        assert result["state"] == "Pending"
        assert result["reason"] in {"POD_SCHEDULED", "POD_PENDING"}

    def test_get_status_keeps_pending_for_mixed_capacity_and_affinity_message(
        self, mock_k8s_client
    ):
        provider = AgentSandboxProvider(mock_k8s_client)
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
                "podTemplate": {
                    "spec": {
                        "nodeSelector": {
                            "kubernetes.io/os": "linux",
                            "kubernetes.io/arch": "arm64",
                        }
                    }
                }
            },
            "status": {"conditions": [], "selector": "app=sandbox"},
            "metadata": {"creationTimestamp": "2025-12-31T09:00:00Z", "namespace": "test-ns"},
        }

        result = provider.get_status(workload)

        assert result["state"] == "Pending"
        assert result["reason"] in {"POD_SCHEDULED", "POD_PENDING"}

    def test_get_endpoint_info_prefers_running_pod(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
        mock_k8s_client.list_pods.return_value = [
            SimpleNamespace(status=SimpleNamespace(phase="Running", pod_ip="10.0.0.9"))
        ]
        workload = {
            "status": {"selector": "app=sandbox"},
            "metadata": {"namespace": "test-ns"},
        }

        endpoint = provider.get_endpoint_info(workload, 8080, "sandbox-123")

        assert endpoint.endpoint == "10.0.0.9:8080"
        assert endpoint.headers is None

    def test_get_endpoint_info_falls_back_to_service_fqdn(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
        mock_k8s_client.list_pods.side_effect = Exception("boom")
        workload = {
            "status": {"selector": "app=sandbox", "serviceFQDN": "svc.example.com"},
            "metadata": {"namespace": "test-ns"},
        }

        endpoint = provider.get_endpoint_info(workload, 9000, "sandbox-123")

        assert endpoint.endpoint == "svc.example.com:9000"
        assert endpoint.headers is None


class TestAgentSandboxProviderExecdInit:
    """AgentSandboxProvider execd init container resource tests"""

    def test_init_container_has_no_resources_when_not_configured(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
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
            expires_at=datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        init_containers = body["spec"]["podTemplate"]["spec"]["initContainers"]
        assert len(init_containers) == 1
        assert "resources" not in init_containers[0]

    def test_init_container_has_resources_when_configured(self, mock_k8s_client):
        provider = AgentSandboxProvider(
            mock_k8s_client,
            _app_config(
                execd_init_resources=ExecdInitResources(
                    limits={"cpu": "100m", "memory": "128Mi"},
                    requests={"cpu": "50m", "memory": "64Mi"},
                )
            ),
        )
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
            expires_at=datetime(2025, 12, 31, 10, 0, 0, tzinfo=timezone.utc),
            execd_image="execd:latest",
        )

        body = mock_k8s_client.create_custom_object.call_args.kwargs["body"]
        init_containers = body["spec"]["podTemplate"]["spec"]["initContainers"]
        assert init_containers[0]["resources"]["limits"] == {"cpu": "100m", "memory": "128Mi"}
        assert init_containers[0]["resources"]["requests"] == {"cpu": "50m", "memory": "64Mi"}


class TestAgentSandboxProviderEgress:
    """AgentSandboxProvider egress sidecar tests"""

    def test_create_workload_without_network_policy_no_sidecar(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
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
        pod_spec = body["spec"]["podTemplate"]["spec"]
        containers = pod_spec["containers"]

        # Should only have main container
        assert len(containers) == 1
        assert containers[0]["name"] == "sandbox"
        # Should not have securityContext with sysctls
        assert "securityContext" not in pod_spec or "sysctls" not in pod_spec.get(
            "securityContext", {}
        )

    def test_create_workload_with_network_policy_adds_sidecar(self, mock_k8s_client):
        provider = AgentSandboxProvider(
            mock_k8s_client,
            _app_config(egress=EgressConfig()),
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
        pod_spec = body["spec"]["podTemplate"]["spec"]
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

    def test_create_workload_with_network_policy_persists_annotation_and_sidecar_token(
        self, mock_k8s_client
    ):
        provider = AgentSandboxProvider(mock_k8s_client)
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

        containers = body["spec"]["podTemplate"]["spec"]["containers"]
        sidecar = next((c for c in containers if c["name"] == "egress"), None)
        assert sidecar is not None
        env_vars = {e["name"]: e["value"] for e in sidecar.get("env", [])}
        assert env_vars[OPENSANDBOX_EGRESS_TOKEN] == "egress-token"
        assert env_vars["OPENSANDBOX_EGRESS_MODE"] == EGRESS_MODE_DNS
        assert sidecar["readinessProbe"]["httpGet"]["httpHeaders"] == [
            {"name": OPEN_SANDBOX_EGRESS_AUTH_HEADER, "value": "egress-token"}
        ]

    def test_create_workload_with_egress_mode_dns_nft(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
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
        containers = body["spec"]["podTemplate"]["spec"]["containers"]
        sidecar = next((c for c in containers if c["name"] == "egress"), None)
        assert sidecar is not None
        env_vars = {e["name"]: e["value"] for e in sidecar.get("env", [])}
        assert env_vars["OPENSANDBOX_EGRESS_MODE"] == EGRESS_MODE_DNS_NFT

    def test_create_workload_with_network_policy_does_not_add_pod_ipv6_sysctls(
        self, mock_k8s_client
    ):
        provider = AgentSandboxProvider(
            mock_k8s_client,
            _app_config(egress=EgressConfig()),
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
        pod_spec = body["spec"]["podTemplate"]["spec"]

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
        """With ``egress.disable_ipv6`` false, execd init stays unprivileged without sysctl writes."""
        provider = AgentSandboxProvider(
            mock_k8s_client,
            _app_config(egress=EgressConfig(disable_ipv6=False)),
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
        pod_spec = body["spec"]["podTemplate"]["spec"]
        execd_init = pod_spec["initContainers"][0]
        assert execd_init["name"] == "execd-installer"
        assert "securityContext" not in execd_init
        assert "/proc/sys/net/ipv6/conf/all/disable_ipv6" not in execd_init["args"][0]

    def test_create_workload_with_network_policy_drops_net_admin_from_main_container(
        self, mock_k8s_client
    ):
        provider = AgentSandboxProvider(mock_k8s_client)
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
        pod_spec = body["spec"]["podTemplate"]["spec"]
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
        provider = AgentSandboxProvider(mock_k8s_client)
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
        pod_spec = body["spec"]["podTemplate"]["spec"]
        containers = pod_spec["containers"]

        # Should only have main container
        assert len(containers) == 1
        assert containers[0]["name"] == "sandbox"

    def test_egress_sidecar_contains_network_policy_in_env(self, mock_k8s_client):
        provider = AgentSandboxProvider(mock_k8s_client)
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
        pod_spec = body["spec"]["podTemplate"]["spec"]
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
        provider = AgentSandboxProvider(mock_k8s_client)
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
        pod_spec = body["spec"]["podTemplate"]["spec"]
        containers = pod_spec["containers"]

        main_container = containers[0]
        # Main container should not have securityContext when no network policy
        assert "securityContext" not in main_container
