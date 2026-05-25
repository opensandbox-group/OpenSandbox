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
Shared fixtures for Kubernetes runtime tests.
"""
from datetime import datetime, timezone, timedelta
from unittest.mock import MagicMock
from typing import Dict, Any

import pytest

from opensandbox_server.api.schema import CreateSandboxRequest, ImageSpec, ResourceLimits
from opensandbox_server.config import KubernetesRuntimeConfig
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.k8s.provider_factory import PROVIDER_TYPE_BATCHSANDBOX


@pytest.fixture
def mock_k8s_client():
    """Provide mocked K8sClient"""
    client = MagicMock(spec=K8sClient)
    mock_custom_api = MagicMock()
    mock_core_api = MagicMock()
    client.get_custom_objects_api.return_value = mock_custom_api
    client.get_core_v1_api.return_value = mock_core_api
    client.custom_api = mock_custom_api
    client.core_api = mock_core_api
    # Unified resource operation methods
    client.create_custom_object = MagicMock(return_value={"metadata": {"name": "test", "uid": "uid"}})
    client.get_custom_object = MagicMock(return_value=None)
    client.list_custom_objects = MagicMock(return_value=[])
    client.delete_custom_object = MagicMock()
    client.patch_custom_object = MagicMock()
    client.create_secret = MagicMock()
    client.list_pods = MagicMock(return_value=[])
    return client


@pytest.fixture
def k8s_runtime_config():
    """Provide test Kubernetes configuration"""
    return KubernetesRuntimeConfig(
        kubeconfig_path="/tmp/test-kubeconfig",
        namespace="test-namespace",
        service_account="test-sa",
        workload_provider=PROVIDER_TYPE_BATCHSANDBOX,
    )


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
def k8s_runtime_config_with_template(tmp_path):
    """Provide Kubernetes configuration with template file"""
    template_file = tmp_path / "template.yaml"
    template_file.write_text("""
metadata:
  annotations:
    managed-by: opensandbox
spec:
  template:
    spec:
      nodeSelector:
        workload: sandbox
      tolerations:
        - operator: Exists
""")
    return KubernetesRuntimeConfig(
        kubeconfig_path="/tmp/test-kubeconfig",
        namespace="test-namespace",
        service_account="test-sa",
        workload_provider=PROVIDER_TYPE_BATCHSANDBOX,
        batchsandbox_template_file=str(template_file),
    )


@pytest.fixture
def valid_batchsandbox_template() -> Dict[str, Any]:
    """Provide valid BatchSandbox template"""
    return {
        "metadata": {
            "annotations": {
                "managed-by": "opensandbox",
                "template-source": "test-template"
            }
        },
        "spec": {
            "template": {
                "spec": {
                    "restartPolicy": "Never",
                    "nodeSelector": {
                        "workload": "sandbox",
                        "environment": "test"
                    },
                    "tolerations": [
                        {
                            "key": "sandbox",
                            "operator": "Equal",
                            "value": "true",
                            "effect": "NoSchedule"
                        }
                    ],
                    "priorityClassName": "sandbox-default"
                }
            }
        }
    }


@pytest.fixture
def sample_create_request():
    """Provide sample create request"""
    return CreateSandboxRequest(
        image=ImageSpec(uri="python:3.11"),
        entrypoint=["/bin/bash", "-c", "sleep 3600"],
        timeout=3600,
        resourceLimits=ResourceLimits(root={"cpu": "1", "memory": "1Gi"}),
        env={"ENV": "test", "DEBUG": "true"},
        metadata={"team": "platform", "project": "test"}
    )


@pytest.fixture
def mock_batchsandbox_response():
    """Provide mocked BatchSandbox response"""
    return {
        "apiVersion": "sandbox.opensandbox.io/v1alpha1",
        "kind": "BatchSandbox",
        "metadata": {
            "name": "test-id",
            "namespace": "test-namespace",
            "creationTimestamp": "2025-12-24T10:00:00Z",
            "uid": "test-uid-12345",
            "annotations": {
                "sandbox.opensandbox.io/endpoints": '["10.0.0.1"]'
            },
            "labels": {
                "opensandbox.io/id": "test-id"
            }
        },
        "spec": {
            "replicas": 1,
            "expireTime": "2025-12-24T11:00:00+00:00",
            "template": {
                "spec": {
                    "containers": [
                        {
                            "name": "sandbox",
                            "image": "python:3.11"
                        }
                    ]
                }
            }
        },
        "status": {
            "replicas": 1,
            "allocated": 1,
            "ready": 1,
            "taskFailed": 0,
            "taskPending": 0,
            "taskRunning": 0,
            "taskSucceed": 0,
            "taskUnknown": 0
        }
    }


@pytest.fixture
def mock_batchsandbox_list_response(mock_batchsandbox_response):
    """Provide mocked BatchSandbox list response"""
    return {
        "apiVersion": "sandbox.opensandbox.io/v1alpha1",
        "kind": "BatchSandboxList",
        "items": [mock_batchsandbox_response]
    }


@pytest.fixture
def fixed_datetime():
    """Provide fixed datetime for testing"""
    return datetime(2025, 12, 24, 10, 0, 0, tzinfo=timezone.utc)


@pytest.fixture
def k8s_app_config(k8s_runtime_config):
    """Provide complete app configuration (Kubernetes type)"""
    from opensandbox_server.config import AppConfig, RuntimeConfig, ServerConfig
    
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
        kubernetes=k8s_runtime_config,
    )


@pytest.fixture
def agent_sandbox_app_config(agent_sandbox_runtime_config):
    """Provide complete app configuration (kubernetes + agent-sandbox provider)"""
    from opensandbox_server.config import AppConfig, RuntimeConfig, ServerConfig, AgentSandboxRuntimeConfig

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
def app_config_no_k8s():
    """Provide app configuration without Kubernetes config"""
    from opensandbox_server.config import AppConfig, RuntimeConfig, ServerConfig
    
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
        kubernetes=None,  # No Kubernetes config
    )


@pytest.fixture
def app_config_docker():
    """Provide Docker type app configuration"""
    from opensandbox_server.config import AppConfig, RuntimeConfig, ServerConfig
    
    return AppConfig(
        server=ServerConfig(
            host="0.0.0.0",
            port=8080,
            api_key="test-api-key",
        ),
        runtime=RuntimeConfig(
            type="docker",  # Docker type
            execd_image="ghcr.io/opensandbox/execd:test",
        ),
        kubernetes=None,
    )


@pytest.fixture
def k8s_service(k8s_app_config):
    """Provide mocked KubernetesSandboxService"""
    from unittest.mock import patch, MagicMock
    
    with patch('opensandbox_server.services.k8s.kubernetes_service.K8sClient') as mock_k8s_client_cls, \
         patch('opensandbox_server.services.k8s.kubernetes_service.create_workload_provider') as mock_create_provider:

        # Mock K8sClient instance
        mock_k8s_client = MagicMock()
        mock_k8s_client_cls.return_value = mock_k8s_client
        
        # Mock WorkloadProvider instance
        mock_provider = MagicMock()
        mock_create_provider.return_value = mock_provider

        from opensandbox_server.services.k8s.kubernetes_service import KubernetesSandboxService
        service = KubernetesSandboxService(k8s_app_config)
        
        # Save mock objects for access in tests
        service.k8s_client = mock_k8s_client
        service.workload_provider = mock_provider

        yield service


@pytest.fixture
def create_sandbox_request():
    """Provide standard sandbox creation request"""
    from opensandbox_server.api.schema import ResourceLimits
    
    return CreateSandboxRequest(
        image=ImageSpec(uri="python:3.9"),
        entrypoint=["/bin/bash", "-c", "sleep infinity"],
        timeout=3600,
        env={"ENV": "test"},
        metadata={"team": "test"},
        resourceLimits=ResourceLimits(root={"cpu": "1", "memory": "1Gi"}),
    )


@pytest.fixture
def mock_workload():
    """Provide mocked workload object"""
    return {
        "metadata": {
            "name": "test-sandbox-123",
            "uid": "abc-123",
            "labels": {
                "opensandbox.io/id": "test-sandbox-123",
            },
            "annotations": {
                "opensandbox.io/created-at": datetime.now(timezone.utc).isoformat(),
                "opensandbox.io/expires-at": (datetime.now(timezone.utc) + timedelta(hours=1)).isoformat(),
                "opensandbox.io/image": '{"uri": "python:3.9"}',
                "opensandbox.io/entrypoint": '["/bin/bash", "-c", "sleep infinity"]',
            },
            "creationTimestamp": datetime.now(timezone.utc).isoformat(),
        },
        "spec": {},
        "status": {
            "state": "Running",
        },
    }


@pytest.fixture
def isolated_registry():
    """
    Fixture to isolate provider registry for each test.

    Saves the original registry before test and restores it after,
    preventing global state pollution.
    """
    from opensandbox_server.services.k8s import provider_factory

    # Save original registry
    original_registry = provider_factory._PROVIDER_REGISTRY.copy()

    yield

    # Restore original registry
    provider_factory._PROVIDER_REGISTRY = original_registry
