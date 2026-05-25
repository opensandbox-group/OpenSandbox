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
from unittest.mock import MagicMock, patch

import pytest
from fastapi import HTTPException
from pydantic import ValidationError

from opensandbox_server.api.schema import SandboxStatus
from opensandbox_server.config import (
    AppConfig,
    RuntimeConfig,
    ServerConfig,
    KubernetesRuntimeConfig,
    AgentSandboxRuntimeConfig,
)
from opensandbox_server.services.k8s.kubernetes_service import KubernetesSandboxService
from opensandbox_server.services.constants import SANDBOX_SNAPSHOT_ID_LABEL, SandboxErrorCodes

@pytest.fixture
def agent_sandbox_runtime_config():
    """Provide agent-sandbox runtime configuration"""
    return KubernetesRuntimeConfig(
        kubeconfig_path="/tmp/test-kubeconfig",
        namespace="test-namespace",
        service_account="test-sa",
        workload_provider="agent-sandbox",
    )

@pytest.fixture
def agent_sandbox_app_config(agent_sandbox_runtime_config):
    """Provide complete app configuration (kubernetes + agent-sandbox provider)"""
    return AppConfig(
        server=ServerConfig(
            host="0.0.0.0",
            port=8080,
            api_key="test-api-key",
        ),
        runtime=RuntimeConfig(
            type="kubernetes",
            execd_image="ghcr.io/opensandbox/execd:test",
        ),
        kubernetes=agent_sandbox_runtime_config,
        agent_sandbox=AgentSandboxRuntimeConfig(
            template_file=None,
            shutdown_policy="Delete",
            ingress_enabled=True,
        ),
    )

@pytest.fixture
def app_config_docker():
    """Provide Docker type app configuration"""
    return AppConfig(
        server=ServerConfig(
            host="0.0.0.0",
            port=8080,
            api_key="test-api-key",
        ),
        runtime=RuntimeConfig(
            type="docker",
            execd_image="ghcr.io/opensandbox/execd:test",
        ),
        kubernetes=None,
    )

class TestAgentSandboxServiceInit:

    def test_init_with_valid_config_succeeds(self, agent_sandbox_runtime_config):
        config = AppConfig(
            server=ServerConfig(
                host="0.0.0.0",
                port=8080,
                api_key="test-api-key",
            ),
            runtime=RuntimeConfig(
                type="kubernetes",
                execd_image="ghcr.io/opensandbox/execd:test",
            ),
            kubernetes=agent_sandbox_runtime_config,
            agent_sandbox=AgentSandboxRuntimeConfig(
                template_file="/tmp/template.yaml",
                shutdown_policy="Retain",
                ingress_enabled=True,
            ),
        )

        with patch("opensandbox_server.services.k8s.kubernetes_service.K8sClient") as mock_k8s_client, patch(
            "opensandbox_server.services.k8s.kubernetes_service.create_workload_provider"
        ) as mock_provider_factory:
            mock_provider_factory.return_value = MagicMock()

            service = KubernetesSandboxService(config)

            assert service.namespace == agent_sandbox_runtime_config.namespace
            assert service.execd_image == config.runtime.execd_image
            mock_k8s_client.assert_called_once_with(agent_sandbox_runtime_config)
            mock_provider_factory.assert_called_once()
            call_kwargs = mock_provider_factory.call_args.kwargs
            assert call_kwargs["provider_type"] == "agent-sandbox"
            assert call_kwargs["app_config"].agent_sandbox.template_file == "/tmp/template.yaml"
            assert call_kwargs["app_config"].agent_sandbox.shutdown_policy == "Retain"
            assert call_kwargs["app_config"].kubernetes == agent_sandbox_runtime_config

    def test_init_without_kubernetes_config_raises_error(self):
        with pytest.raises(ValidationError, match="agent_sandbox block requires kubernetes.workload_provider"):
            AppConfig(
                server=ServerConfig(
                    host="0.0.0.0",
                    port=8080,
                    api_key="test-api-key",
                ),
                runtime=RuntimeConfig(
                    type="kubernetes",
                    execd_image="ghcr.io/opensandbox/execd:test",
                ),
                kubernetes=None,
                agent_sandbox=AgentSandboxRuntimeConfig(),
            )

    def test_init_with_wrong_runtime_type_raises_error(self, app_config_docker):
        with pytest.raises(ValueError, match="requires runtime.type = 'kubernetes'"):
            KubernetesSandboxService(app_config_docker)

    def test_init_with_k8s_client_failure_raises_http_exception(self, agent_sandbox_app_config):
        with patch("opensandbox_server.services.k8s.kubernetes_service.K8sClient") as mock_k8s_client:
            mock_k8s_client.side_effect = Exception("Failed to load kubeconfig")

            with pytest.raises(HTTPException) as exc_info:
                KubernetesSandboxService(agent_sandbox_app_config)

            assert exc_info.value.status_code == 503
            assert "code" in exc_info.value.detail
            assert exc_info.value.detail["code"] == SandboxErrorCodes.K8S_INITIALIZATION_ERROR

class TestAgentSandboxServiceBuildSandbox:

    def test_build_sandbox_from_workload_dict(self):
        service = object.__new__(KubernetesSandboxService)
        service.namespace = "test-namespace"
        service.workload_provider = MagicMock(
            get_workload=MagicMock(),
            get_expiration=MagicMock(return_value=datetime(2025, 12, 31, tzinfo=timezone.utc)),
            get_status=MagicMock(
                return_value={
                    "state": "Running",
                    "reason": "Ready",
                    "message": "Ready",
                    "last_transition_at": datetime(2025, 12, 31, tzinfo=timezone.utc),
                }
            ),
        )

        workload = {
            "metadata": {
                "labels": {
                    "opensandbox.io/id": "sandbox-id",
                    "opensandbox.io.evil/key": "public",
                    "team": "platform",
                },
                "creationTimestamp": "2025-12-31T09:00:00Z",
            },
            "spec": {
                "podTemplate": {
                    "spec": {
                        "containers": [
                            {
                                "image": "python:3.11",
                                "command": ["/bin/bash"],
                            }
                        ]
                    }
                }
            },
        }
        service.workload_provider.get_workload.return_value = workload

        sandbox = service.get_sandbox("sandbox-id")

        assert sandbox.id == "sandbox-id"
        assert sandbox.image.uri == "python:3.11"
        assert sandbox.entrypoint == ["/bin/bash"]
        assert sandbox.metadata == {"opensandbox.io.evil/key": "public", "team": "platform"}
        assert isinstance(sandbox.status, SandboxStatus)
        assert sandbox.status.state == "Running"

    def test_build_snapshot_restored_sandbox_from_workload_dict(self):
        service = object.__new__(KubernetesSandboxService)
        service.namespace = "test-namespace"
        service.workload_provider = MagicMock(
            get_workload=MagicMock(),
            get_expiration=MagicMock(return_value=datetime(2025, 12, 31, tzinfo=timezone.utc)),
            get_status=MagicMock(
                return_value={
                    "state": "Running",
                    "reason": "Ready",
                    "message": "Ready",
                    "last_transition_at": datetime(2025, 12, 31, tzinfo=timezone.utc),
                }
            ),
        )

        workload = {
            "metadata": {
                "labels": {
                    "opensandbox.io/id": "sandbox-id",
                    SANDBOX_SNAPSHOT_ID_LABEL: "snap-001",
                    "team": "platform",
                },
                "creationTimestamp": "2025-12-31T09:00:00Z",
            },
            "spec": {
                "podTemplate": {
                    "spec": {
                        "containers": [
                            {
                                "image": "opensandbox-snapshots:snap-001",
                                "command": ["tail", "-f", "/dev/null"],
                            }
                        ]
                    }
                }
            },
        }
        service.workload_provider.get_workload.return_value = workload

        sandbox = service.get_sandbox("sandbox-id")

        assert sandbox.snapshot_id == "snap-001"
        assert sandbox.image is None
