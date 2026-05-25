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
from unittest.mock import patch

from opensandbox_server.config import AgentSandboxRuntimeConfig
from opensandbox_server.services.k8s.provider_factory import (
    register_provider,
    create_workload_provider,
    list_available_providers,
    PROVIDER_TYPE_BATCHSANDBOX,
    PROVIDER_TYPE_AGENT_SANDBOX,
)
from opensandbox_server.services.k8s.workload_provider import WorkloadProvider
from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider
from opensandbox_server.services.k8s.agent_sandbox_provider import AgentSandboxProvider

class TestProviderFactory:
    
    def test_register_and_create_batchsandbox_provider(self, mock_k8s_client, k8s_app_config):
        provider = create_workload_provider(
            PROVIDER_TYPE_BATCHSANDBOX,
            mock_k8s_client,
            k8s_app_config,
        )
        
        assert isinstance(provider, BatchSandboxProvider)
        assert provider.k8s_client == mock_k8s_client

    def test_register_and_create_agent_sandbox_provider(
        self,
        mock_k8s_client,
        agent_sandbox_app_config,
        tmp_path,
    ):
        template_file = tmp_path / "agent_sandbox_template.yaml"
        template_file.write_text(
            """
metadata:
  annotations:
    managed-by: opensandbox
spec:
  podTemplate:
    spec:
      nodeSelector:
        workload: sandbox
"""
        )

        agent_config = AgentSandboxRuntimeConfig(
            template_file=str(template_file),
            shutdown_policy="Retain",
            ingress_enabled=True,
        )
        agent_sandbox_app_config.agent_sandbox = agent_config
        provider = create_workload_provider(
            PROVIDER_TYPE_AGENT_SANDBOX,
            mock_k8s_client,
            agent_sandbox_app_config,
        )

        assert isinstance(provider, AgentSandboxProvider)
        assert provider.k8s_client == mock_k8s_client
        assert provider.shutdown_policy == "Retain"
        assert provider.service_account == agent_sandbox_app_config.kubernetes.service_account
    
    def test_create_provider_case_insensitive(self, mock_k8s_client, k8s_app_config):
        provider1 = create_workload_provider("BatchSandbox", mock_k8s_client, k8s_app_config)
        provider2 = create_workload_provider(PROVIDER_TYPE_BATCHSANDBOX, mock_k8s_client, k8s_app_config)
        provider3 = create_workload_provider("BATCHSANDBOX", mock_k8s_client, k8s_app_config)
        
        assert isinstance(provider1, BatchSandboxProvider)
        assert isinstance(provider2, BatchSandboxProvider)
        assert isinstance(provider3, BatchSandboxProvider)
    
    def test_create_provider_with_none_type_uses_default(self, mock_k8s_client, k8s_app_config):
        provider = create_workload_provider(None, mock_k8s_client, k8s_app_config)
        
        # Should use the first registered provider (batchsandbox)
        assert isinstance(provider, BatchSandboxProvider)
    
    def test_create_provider_with_invalid_type_raises_error(self, mock_k8s_client):
        with pytest.raises(ValueError, match="Unsupported workload provider type"):
            create_workload_provider("invalid", mock_k8s_client)
    
    def test_create_batchsandbox_with_template_file(self, mock_k8s_client, k8s_app_config, tmp_path):
        template_file = tmp_path / "test_template.yaml"
        template_file.write_text("""apiVersion: execution.alibaba-inc.com/v1alpha1
kind: BatchSandbox
metadata:
  name: test-template
spec:
  template:
    spec:
      nodeSelector:
        gpu: "true"
""")

        k8s_app_config.kubernetes.batchsandbox_template_file = str(template_file)

        with patch.object(BatchSandboxProvider, '__init__', return_value=None) as mock_init:
            create_workload_provider(PROVIDER_TYPE_BATCHSANDBOX, mock_k8s_client, k8s_app_config)
            
            # Verify that app_config carrying the template path was passed
            mock_init.assert_called_once()
            call_kwargs = mock_init.call_args.kwargs
            assert call_kwargs['app_config'].kubernetes.batchsandbox_template_file == str(template_file)
    
    def test_list_available_providers(self):
        providers = list_available_providers()

        assert isinstance(providers, list)
        assert PROVIDER_TYPE_BATCHSANDBOX in providers
        assert PROVIDER_TYPE_AGENT_SANDBOX in providers
    
    def test_register_custom_provider(self, mock_k8s_client, isolated_registry):
        # Create a custom provider class
        class CustomProvider(WorkloadProvider):
            def __init__(self, k8s_client):
                self.k8s_client = k8s_client
            
            def create_workload(self, *args, **kwargs):
                pass
            
            def get_workload(self, *args, **kwargs):
                pass
            
            def delete_workload(self, *args, **kwargs):
                pass
            
            def list_workloads(self, *args, **kwargs):
                pass
            
            def update_expiration(self, *args, **kwargs):
                pass
            
            def get_expiration(self, *args, **kwargs):
                pass
            
            def get_status(self, *args, **kwargs):
                pass
            
            def get_endpoint_info(self, *args, **kwargs):
                pass
        
        # Register custom provider
        register_provider("custom", CustomProvider)
        
        # Verify that custom provider can be created
        provider = create_workload_provider("custom", mock_k8s_client)
        assert isinstance(provider, CustomProvider)
        
        # Verify it's registered
        assert "custom" in list_available_providers()
    
    def test_create_batchsandbox_with_config(self, mock_k8s_client, k8s_app_config):
        provider = create_workload_provider(PROVIDER_TYPE_BATCHSANDBOX, mock_k8s_client, k8s_app_config)
        
        assert isinstance(provider, BatchSandboxProvider)
        assert provider.k8s_client == mock_k8s_client
    
    def test_create_provider_with_empty_registry_raises_error(self, mock_k8s_client, isolated_registry):
        from opensandbox_server.services.k8s import provider_factory
        
        # Clear the registry to test empty registry scenario
        provider_factory._PROVIDER_REGISTRY.clear()
        
        # Verify that ValueError is raised when registry is empty and type is None
        with pytest.raises(ValueError, match="No workload providers are registered"):
            create_workload_provider(None, mock_k8s_client)
