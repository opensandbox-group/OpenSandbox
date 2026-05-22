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
from datetime import datetime, timedelta, timezone
from unittest.mock import MagicMock, patch
from fastapi import HTTPException

from opensandbox_server.services.k8s.kubernetes_service import KubernetesSandboxService
from opensandbox_server.services.constants import (
    OPEN_SANDBOX_EGRESS_AUTH_HEADER,
    OPEN_SANDBOX_SECURE_ACCESS_HEADER,
    SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY,
    SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY,
    SANDBOX_MANUAL_CLEANUP_LABEL,
    SandboxErrorCodes,
)
from opensandbox_server.api.schema import ImageAuth, ListSandboxesRequest, NetworkPolicy, PlatformSpec
from opensandbox_server.config import (
    EGRESS_MODE_DNS,
    EGRESS_MODE_DNS_NFT,
    EgressConfig,
    GatewayConfig,
    GatewayRouteModeConfig,
    IngressConfig,
    SecureAccessConfig,
    SecureAccessKey,
)
from opensandbox_server.api.schema import Endpoint

class TestKubernetesSandboxServiceInit:
    
    def test_init_with_valid_config_succeeds(self, k8s_app_config):
        with patch('opensandbox_server.services.k8s.kubernetes_service.K8sClient') as mock_k8s_client, \
             patch('opensandbox_server.services.k8s.kubernetes_service.create_workload_provider') as mock_create_provider:
            
            mock_provider = MagicMock()
            mock_create_provider.return_value = mock_provider
            
            service = KubernetesSandboxService(k8s_app_config)
            
            assert service.namespace == k8s_app_config.kubernetes.namespace
            assert service.execd_image == k8s_app_config.runtime.execd_image
            mock_k8s_client.assert_called_once_with(k8s_app_config.kubernetes)
            mock_create_provider.assert_called_once()
    
    def test_init_without_kubernetes_config_raises_error(self, app_config_no_k8s):
        # app_config_no_k8s still has kubernetes config, just without kubeconfig
        # This will cause K8sClient initialization to fail and raise HTTPException
        with pytest.raises(HTTPException) as exc_info:
            KubernetesSandboxService(app_config_no_k8s)
        
        assert exc_info.value.status_code == 503
        assert exc_info.value.detail["code"] == SandboxErrorCodes.K8S_INITIALIZATION_ERROR
    
    def test_init_with_wrong_runtime_type_raises_error(self, app_config_docker):
        with pytest.raises(ValueError, match="requires runtime.type = 'kubernetes'"):
            KubernetesSandboxService(app_config_docker)
    
    def test_init_with_k8s_client_failure_raises_http_exception(self, k8s_app_config):
        with patch('opensandbox_server.services.k8s.kubernetes_service.K8sClient') as mock_k8s_client:
            mock_k8s_client.side_effect = Exception("Failed to load kubeconfig")
            
            with pytest.raises(HTTPException) as exc_info:
                KubernetesSandboxService(k8s_app_config)
            
            assert exc_info.value.status_code == 503
            assert "code" in exc_info.value.detail
            assert exc_info.value.detail["code"] == SandboxErrorCodes.K8S_INITIALIZATION_ERROR

class TestKubernetesSandboxServiceCreate:
    
    @pytest.mark.asyncio
    async def test_create_sandbox_with_valid_request_succeeds(
        self, k8s_service, create_sandbox_request, mock_workload
    ):
        # Mock workload provider
        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-sandbox-123",
            "uid": "abc-123",
        }
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Pod is running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        k8s_service.workload_provider.get_endpoint_info.return_value = "10.244.0.5:8080"
        k8s_service.workload_provider.get_expiration.return_value = datetime.now(timezone.utc) + timedelta(hours=1)
        
        response = await k8s_service.create_sandbox(create_sandbox_request)
        
        # CreateSandboxResponse uses 'id' field
        assert response.id is not None
        assert response.status.state == "Running"
        k8s_service.workload_provider.create_workload.assert_called_once()

    @pytest.mark.asyncio
    async def test_create_sandbox_normalizes_allocated_status_to_running(
        self, k8s_service, create_sandbox_request, mock_workload
    ):
        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-sandbox-123",
            "uid": "abc-123",
        }
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Allocated",
            "reason": "IP_ASSIGNED",
            "message": "Pod has IP assigned but not ready",
            "last_transition_at": datetime.now(timezone.utc),
        }

        response = await k8s_service.create_sandbox(create_sandbox_request)

        assert response.status.state == "Running"
        assert response.status.reason == "IP_ASSIGNED"
        assert response.status.message == "Pod has IP assigned and sandbox is ready for requests"

    @pytest.mark.asyncio
    async def test_create_sandbox_uses_configured_timeout_and_poll_interval(
        self, k8s_service, create_sandbox_request, mock_workload
    ):

        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-sandbox-123",
            "uid": "abc-123",
        }
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Pod is running",
            "last_transition_at": datetime.now(timezone.utc),
        }

        # Override config values
        k8s_service.app_config.kubernetes.sandbox_create_timeout_seconds = 120
        k8s_service.app_config.kubernetes.sandbox_create_poll_interval_seconds = 0.5

        with patch.object(k8s_service, "_wait_for_sandbox_ready", wraps=k8s_service._wait_for_sandbox_ready) as mock_wait:
            await k8s_service.create_sandbox(create_sandbox_request)

        mock_wait.assert_called_once()
        _, kwargs = mock_wait.call_args
        assert kwargs["timeout_seconds"] == 120
        assert kwargs["poll_interval_seconds"] == 0.5

    @pytest.mark.asyncio
    async def test_create_sandbox_rejects_image_auth_when_provider_not_supported(
        self, k8s_service, create_sandbox_request
    ):
        k8s_service.workload_provider.supports_image_auth.return_value = False
        create_sandbox_request.image.auth = ImageAuth(
            username="registry-user",
            password="registry-pass",
        )

        with pytest.raises(HTTPException) as exc_info:
            await k8s_service.create_sandbox(create_sandbox_request)

        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
        k8s_service.workload_provider.create_workload.assert_not_called()

    @pytest.mark.asyncio
    async def test_create_sandbox_allows_image_auth_when_provider_supported(
        self, k8s_service, create_sandbox_request
    ):
        k8s_service.workload_provider.supports_image_auth.return_value = True
        create_sandbox_request.image.auth = ImageAuth(
            username="registry-user",
            password="registry-pass",
        )
        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-id", "uid": "uid-1"
        }
        k8s_service.workload_provider.get_workload.return_value = MagicMock()
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running", "reason": "", "message": "",
            "last_transition_at": datetime.now(timezone.utc),
        }

        # Should not raise
        await k8s_service.create_sandbox(create_sandbox_request)
        k8s_service.workload_provider.create_workload.assert_called_once()

    @pytest.mark.asyncio
    async def test_create_sandbox_with_no_timeout_calls_provider_with_expires_at_none_and_manual_cleanup_label(
        self, k8s_service, create_sandbox_request
    ):
        """When timeout is None (manual cleanup), provider receives expires_at=None and manual-cleanup label."""
        create_sandbox_request.timeout = None
        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-id", "uid": "uid-1"
        }
        k8s_service.workload_provider.get_workload.return_value = MagicMock()
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running", "reason": "", "message": "",
            "last_transition_at": datetime.now(timezone.utc),
        }

        await k8s_service.create_sandbox(create_sandbox_request)

        k8s_service.workload_provider.create_workload.assert_called_once()
        _, kwargs = k8s_service.workload_provider.create_workload.call_args
        assert kwargs["expires_at"] is None
        assert kwargs["labels"].get(SANDBOX_MANUAL_CLEANUP_LABEL) == "true"

    @pytest.mark.asyncio
    async def test_create_sandbox_with_network_policy_passes_egress_token_and_annotations(
        self, k8s_service, create_sandbox_request
    ):
        create_sandbox_request.network_policy = NetworkPolicy(default_action="deny", egress=[])
        k8s_service.app_config.egress = EgressConfig(image="opensandbox/egress:v1.0.12")
        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-id", "uid": "uid-1"
        }
        k8s_service.workload_provider.get_workload.return_value = MagicMock()
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running", "reason": "", "message": "",
            "last_transition_at": datetime.now(timezone.utc),
        }

        with patch(
            "opensandbox_server.services.k8s.kubernetes_service.generate_egress_token",
            return_value="egress-token",
        ):
            await k8s_service.create_sandbox(create_sandbox_request)

        _, kwargs = k8s_service.workload_provider.create_workload.call_args
        assert kwargs["egress_auth_token"] == "egress-token"
        assert kwargs["egress_mode"] == EGRESS_MODE_DNS
        assert kwargs["annotations"][SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY] == "egress-token"

    @pytest.mark.asyncio
    async def test_create_sandbox_with_secure_access_passes_annotations(
        self, k8s_service, create_sandbox_request
    ):
        create_sandbox_request.secure_access = True
        k8s_service.app_config.ingress = IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="gateway.example.com",
                route=GatewayRouteModeConfig(mode="header"),
            ),
        )
        k8s_service.ingress_config = k8s_service.app_config.ingress
        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-id", "uid": "uid-1"
        }
        k8s_service.workload_provider.get_workload.return_value = MagicMock()
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running", "reason": "", "message": "",
            "last_transition_at": datetime.now(timezone.utc),
        }

        with patch(
            "opensandbox_server.services.k8s.kubernetes_service.generate_secure_access_token",
            return_value="secure-token",
        ):
            await k8s_service.create_sandbox(create_sandbox_request)

        _, kwargs = k8s_service.workload_provider.create_workload.call_args
        assert kwargs["annotations"][SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY] == "secure-token"

    @pytest.mark.asyncio
    async def test_create_sandbox_rejects_secure_access_without_gateway_ingress(
        self, k8s_service, create_sandbox_request
    ):
        create_sandbox_request.secure_access = True
        k8s_service.app_config.ingress = IngressConfig(mode="direct")
        k8s_service.ingress_config = k8s_service.app_config.ingress

        with pytest.raises(HTTPException) as exc_info:
            await k8s_service.create_sandbox(create_sandbox_request)

        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
        assert "ingress.mode='gateway'" in exc_info.value.detail["message"]
        k8s_service.workload_provider.create_workload.assert_not_called()

    @pytest.mark.asyncio
    async def test_create_sandbox_with_network_policy_passes_egress_mode_dns_nft_from_config(
        self, k8s_service, create_sandbox_request
    ):
        create_sandbox_request.network_policy = NetworkPolicy(default_action="deny", egress=[])
        k8s_service.app_config.egress = EgressConfig(
            image="opensandbox/egress:v1.0.12",
            mode=EGRESS_MODE_DNS_NFT,
        )
        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-id", "uid": "uid-1"
        }
        k8s_service.workload_provider.get_workload.return_value = MagicMock()
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running", "reason": "", "message": "",
            "last_transition_at": datetime.now(timezone.utc),
        }

        with patch(
            "opensandbox_server.services.k8s.kubernetes_service.generate_egress_token",
            return_value="egress-token",
        ):
            await k8s_service.create_sandbox(create_sandbox_request)

        _, kwargs = k8s_service.workload_provider.create_workload.call_args
        assert kwargs["egress_mode"] == EGRESS_MODE_DNS_NFT

    @pytest.mark.asyncio
    async def test_create_sandbox_passes_platform_to_workload_provider(
        self, k8s_service, create_sandbox_request
    ):
        create_sandbox_request.platform = PlatformSpec(os="linux", arch="arm64")
        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-id", "uid": "uid-1"
        }
        k8s_service.workload_provider.get_workload.return_value = {
            "spec": {
                "template": {
                    "spec": {
                        "nodeSelector": {
                            "kubernetes.io/os": "linux",
                            "kubernetes.io/arch": "arm64",
                        }
                    }
                }
            }
        }
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running", "reason": "", "message": "",
            "last_transition_at": datetime.now(timezone.utc),
        }

        response = await k8s_service.create_sandbox(create_sandbox_request)

        _, kwargs = k8s_service.workload_provider.create_workload.call_args
        assert kwargs["platform"] == create_sandbox_request.platform
        assert response.platform is not None
        assert response.platform.os == "linux"
        assert response.platform.arch == "arm64"

    @pytest.mark.asyncio
    async def test_create_sandbox_rejects_unsupported_platform(self, k8s_service, create_sandbox_request):
        create_sandbox_request.platform = PlatformSpec(os="darwin", arch="arm64")

        with pytest.raises(HTTPException) as exc_info:
            await k8s_service.create_sandbox(create_sandbox_request)

        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
        k8s_service.workload_provider.create_workload.assert_not_called()

    @pytest.mark.asyncio
    async def test_create_sandbox_derives_platform_from_node_affinity(
        self, k8s_service, create_sandbox_request
    ):
        create_sandbox_request.platform = None
        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-id", "uid": "uid-1"
        }
        k8s_service.workload_provider.get_workload.return_value = {
            "spec": {
                "template": {
                    "spec": {
                        "affinity": {
                            "nodeAffinity": {
                                "requiredDuringSchedulingIgnoredDuringExecution": {
                                    "nodeSelectorTerms": [
                                        {
                                            "matchExpressions": [
                                                {
                                                    "key": "kubernetes.io/os",
                                                    "operator": "In",
                                                    "values": ["linux"],
                                                },
                                                {
                                                    "key": "kubernetes.io/arch",
                                                    "operator": "In",
                                                    "values": ["arm64"],
                                                },
                                            ]
                                        }
                                    ]
                                }
                            }
                        }
                    }
                }
            }
        }
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running", "reason": "", "message": "",
            "last_transition_at": datetime.now(timezone.utc),
        }

        response = await k8s_service.create_sandbox(create_sandbox_request)

        assert response.platform is not None
        assert response.platform.os == "linux"
        assert response.platform.arch == "arm64"

    @pytest.mark.asyncio
    async def test_create_sandbox_uses_template_platform_constraints(
        self, k8s_service, create_sandbox_request
    ):
        create_sandbox_request.platform = PlatformSpec(os="linux", arch="arm64")
        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-id", "uid": "uid-1"
        }
        k8s_service.workload_provider.get_workload.return_value = {
            "spec": {
                "template": {
                    "spec": {
                        "nodeSelector": {
                            "kubernetes.io/os": "linux",
                            "kubernetes.io/arch": "arm64",
                        }
                    }
                }
            }
        }
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running", "reason": "", "message": "",
            "last_transition_at": datetime.now(timezone.utc),
        }
        response = await k8s_service.create_sandbox(create_sandbox_request)

        assert response.platform is not None
        assert response.platform.os == "linux"
        assert response.platform.arch == "arm64"

    def test_get_endpoint_merges_egress_auth_header_from_instance_metadata(
        self, k8s_service
    ):
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {
                "annotations": {
                    SANDBOX_EGRESS_AUTH_TOKEN_METADATA_KEY: "egress-token",
                }
            }
        }
        k8s_service.workload_provider.get_endpoint_info.return_value = Endpoint(
            endpoint="gateway.example.com",
            headers={"OpenSandbox-Ingress-To": "sbx-123-44772"},
        )

        endpoint = k8s_service.get_endpoint("sbx-123", 44772)

        assert endpoint.endpoint == "gateway.example.com"
        assert endpoint.headers == {
            "OpenSandbox-Ingress-To": "sbx-123-44772",
            OPEN_SANDBOX_EGRESS_AUTH_HEADER: "egress-token",
        }

    def test_get_execd_endpoint_merges_secure_access_header_from_instance_metadata(
        self, k8s_service
    ):
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {
                "annotations": {
                    SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY: "secure-token",
                }
            }
        }
        k8s_service.workload_provider.get_endpoint_info.return_value = Endpoint(
            endpoint="gateway.example.com",
            headers={"OpenSandbox-Ingress-To": "sbx-123-44772"},
        )

        endpoint = k8s_service.get_endpoint("sbx-123", 44772)

        assert endpoint.endpoint == "gateway.example.com"
        assert endpoint.headers == {
            "OpenSandbox-Ingress-To": "sbx-123-44772",
            OPEN_SANDBOX_SECURE_ACCESS_HEADER: "secure-token",
        }

    def test_get_user_endpoint_also_merges_secure_access_header(
        self, k8s_service
    ):
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {
                "annotations": {
                    SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY: "secure-token",
                }
            }
        }
        k8s_service.workload_provider.get_endpoint_info.return_value = Endpoint(
            endpoint="gateway.example.com",
            headers={"OpenSandbox-Ingress-To": "sbx-123-8080"},
        )

        endpoint = k8s_service.get_endpoint("sbx-123", 8080)

        assert endpoint.headers == {
            "OpenSandbox-Ingress-To": "sbx-123-8080",
            OPEN_SANDBOX_SECURE_ACCESS_HEADER: "secure-token",
        }

    @pytest.mark.asyncio
    async def test_create_sandbox_rejects_timeout_above_configured_maximum(
        self, k8s_service, create_sandbox_request
    ):
        k8s_service.app_config.server.max_sandbox_timeout_seconds = 3600
        create_sandbox_request.timeout = 7200

        with pytest.raises(HTTPException) as exc_info:
            await k8s_service.create_sandbox(create_sandbox_request)

        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
        assert "configured maximum of 3600s" in exc_info.value.detail["message"]
        k8s_service.workload_provider.create_workload.assert_not_called()

    @pytest.mark.asyncio
    async def test_create_sandbox_pool_mode_skips_image_and_entrypoint_validation(
        self, k8s_service, mock_workload
    ):
        """Pool mode: poolRef only, no image/entrypoint/resourceLimits — should succeed."""
        from opensandbox_server.api.schema import CreateSandboxRequest

        pool_request = CreateSandboxRequest(
            extensions={"poolRef": "my-pool"},
        )

        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-sandbox-pool",
            "uid": "pool-123",
        }
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Pod is running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        k8s_service.workload_provider.get_endpoint_info.return_value = "10.244.0.5:8080"
        k8s_service.workload_provider.get_expiration.return_value = datetime.now(timezone.utc) + timedelta(hours=1)

        response = await k8s_service.create_sandbox(pool_request)

        assert response.id is not None
        assert response.status.state == "Running"
        k8s_service.workload_provider.create_workload.assert_called_once()

    @pytest.mark.asyncio
    async def test_create_sandbox_pool_mode_image_auth_guard_no_error(
        self, k8s_service, mock_workload
    ):
        """Pool mode with image=None should not raise AttributeError in _ensure_image_auth_support."""
        from opensandbox_server.api.schema import CreateSandboxRequest

        pool_request = CreateSandboxRequest(
            extensions={"poolRef": "my-pool"},
        )
        assert pool_request.image is None

        k8s_service.workload_provider.create_workload.return_value = {
            "name": "test-sandbox-pool2",
            "uid": "pool-456",
        }
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Pod is running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        k8s_service.workload_provider.get_endpoint_info.return_value = "10.244.0.6:8080"
        k8s_service.workload_provider.get_expiration.return_value = datetime.now(timezone.utc) + timedelta(hours=1)

        # Should not raise AttributeError on None.auth
        response = await k8s_service.create_sandbox(pool_request)
        assert response.id is not None

class TestWaitForSandboxReady:
    """_wait_for_sandbox_ready method tests"""
    
    @pytest.mark.asyncio
    async def test_wait_for_running_pod_succeeds(self, k8s_service, mock_workload):
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Pod is running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        
        result = await k8s_service._wait_for_sandbox_ready("test-sandbox-id", timeout_seconds=10)
        
        assert result == mock_workload
    
    @pytest.mark.asyncio
    async def test_wait_for_pending_then_running_succeeds(self, k8s_service, mock_workload):
        # Mock state transition: Pending -> Allocated -> Running
        status_sequence = [
            {"state": "Pending", "reason": "", "message": "Pending", "last_transition_at": datetime.now(timezone.utc)},
            {"state": "Allocated", "reason": "IP_ASSIGNED", "message": "IP assigned", "last_transition_at": datetime.now(timezone.utc)},
            {"state": "Running", "reason": "", "message": "Running", "last_transition_at": datetime.now(timezone.utc)},
        ]
        
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.side_effect = status_sequence
        
        result = await k8s_service._wait_for_sandbox_ready("test-sandbox-id", timeout_seconds=10, poll_interval_seconds=0.1)
        
        assert result == mock_workload
        assert k8s_service.workload_provider.get_status.call_count == 2
    
    @pytest.mark.asyncio
    async def test_wait_for_allocated_pod_returns_immediately(self, k8s_service, mock_workload):
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Allocated",
            "reason": "IP_ASSIGNED",
            "message": "Pod has IP assigned",
            "last_transition_at": datetime.now(timezone.utc),
        }
        
        result = await k8s_service._wait_for_sandbox_ready("test-sandbox-id", timeout_seconds=10)
        
        assert result == mock_workload
    
    @pytest.mark.asyncio
    async def test_wait_timeout_raises_exception(self, k8s_service, mock_workload):
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Pending",
            "reason": "",
            "message": "Still pending",
            "last_transition_at": datetime.now(timezone.utc),
        }
        
        with pytest.raises(HTTPException) as exc_info:
            await k8s_service._wait_for_sandbox_ready("test-sandbox-id", timeout_seconds=1, poll_interval_seconds=0.5)
        
        assert exc_info.value.status_code == 504  # Gateway Timeout
        assert "timeout" in exc_info.value.detail["message"].lower()

    @pytest.mark.asyncio
    async def test_wait_returns_400_when_scheduler_marks_platform_unschedulable(self, k8s_service, mock_workload):
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Failed",
            "reason": "POD_PLATFORM_UNSCHEDULABLE",
            "message": "0/1 nodes are available: 1 node(s) didn't match Pod's node affinity.",
            "last_transition_at": datetime.now(timezone.utc),
        }

        with pytest.raises(HTTPException) as exc_info:
            await k8s_service._wait_for_sandbox_ready(
                "test-sandbox-id",
                timeout_seconds=10,
                poll_interval_seconds=0.1,
            )

        assert exc_info.value.status_code == 400
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_PARAMETER
        assert "unschedulable" in exc_info.value.detail["message"].lower()

    @pytest.mark.asyncio
    async def test_wait_keeps_polling_for_generic_failed_scheduling(self, k8s_service, mock_workload):
        status_sequence = [
            {
                "state": "Pending",
                "reason": "FailedScheduling",
                "message": "0/1 nodes are available: 1 Insufficient cpu.",
                "last_transition_at": datetime.now(timezone.utc),
            },
            {
                "state": "Running",
                "reason": "",
                "message": "Running",
                "last_transition_at": datetime.now(timezone.utc),
            },
        ]
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.side_effect = status_sequence

        result = await k8s_service._wait_for_sandbox_ready(
            "test-sandbox-id",
            timeout_seconds=10,
            poll_interval_seconds=0.1,
        )

        assert result == mock_workload

class TestKubernetesSandboxServiceRenew:
    def test_renew_expiration_rejects_manual_cleanup_sandbox(self, k8s_service):
        k8s_service.workload_provider.get_workload.return_value = MagicMock()
        k8s_service.workload_provider.get_expiration.return_value = None
        request = MagicMock(expires_at=datetime.now(timezone.utc) + timedelta(hours=1))

        with pytest.raises(HTTPException) as exc_info:
            k8s_service.renew_expiration("test-sandbox-id", request)

        assert exc_info.value.status_code == 409
        assert exc_info.value.detail["code"] == SandboxErrorCodes.INVALID_EXPIRATION
        assert (
            exc_info.value.detail["message"]
            == "Sandbox test-sandbox-id does not have automatic expiration enabled."
        )

class TestGetSandbox:
    """get_sandbox method tests"""
    
    def test_get_existing_sandbox_succeeds(self, k8s_service, mock_workload):
        mock_workload["spec"] = {
            "template": {
                "spec": {
                    "nodeSelector": {
                        "kubernetes.io/os": "linux",
                        "kubernetes.io/arch": "amd64",
                    }
                }
            }
        }
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        k8s_service.workload_provider.get_endpoint_info.return_value = "10.0.0.1:8080"
        k8s_service.workload_provider.get_expiration.return_value = datetime.now(timezone.utc) + timedelta(hours=1)
        
        # Use sandbox_id from mock_workload
        sandbox = k8s_service.get_sandbox("test-sandbox-123")
        
        # Sandbox uses 'id' field
        assert sandbox.id == "test-sandbox-123"
        assert sandbox.status.state == "Running"
        assert sandbox.platform is not None
        assert sandbox.platform.os == "linux"
        assert sandbox.platform.arch == "amd64"
    
    def test_get_nonexistent_sandbox_raises_404(self, k8s_service):
        k8s_service.workload_provider.get_workload.return_value = None
        
        with pytest.raises(HTTPException) as exc_info:
            k8s_service.get_sandbox("nonexistent-id")
        
        assert exc_info.value.status_code == 404
        assert "not found" in exc_info.value.detail["message"].lower()

    def test_get_sandbox_extracts_platform_from_affinity(self, k8s_service, mock_workload):
        mock_workload["spec"] = {
            "template": {
                "spec": {
                    "affinity": {
                        "nodeAffinity": {
                            "requiredDuringSchedulingIgnoredDuringExecution": {
                                "nodeSelectorTerms": [
                                    {
                                        "matchExpressions": [
                                            {
                                                "key": "kubernetes.io/os",
                                                "operator": "In",
                                                "values": ["linux"],
                                            },
                                            {
                                                "key": "kubernetes.io/arch",
                                                "operator": "In",
                                                "values": ["amd64"],
                                            },
                                        ]
                                    }
                                ]
                            }
                        }
                    }
                }
            }
        }
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        k8s_service.workload_provider.get_expiration.return_value = datetime.now(timezone.utc) + timedelta(hours=1)

        sandbox = k8s_service.get_sandbox("test-sandbox-123")

        assert sandbox.platform is not None
        assert sandbox.platform.os == "linux"
        assert sandbox.platform.arch == "amd64"

    def test_get_sandbox_uses_template_platform_constraints(self, k8s_service, mock_workload):
        mock_workload["spec"] = {
            "template": {
                "spec": {
                    "nodeSelector": {
                        "kubernetes.io/os": "linux",
                        "kubernetes.io/arch": "arm64",
                    }
                }
            }
        }
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        k8s_service.workload_provider.get_expiration.return_value = datetime.now(timezone.utc) + timedelta(hours=1)
        sandbox = k8s_service.get_sandbox("test-sandbox-123")

        assert sandbox.platform is not None
        assert sandbox.platform.os == "linux"
        assert sandbox.platform.arch == "arm64"

    def test_get_sandbox_merges_selector_and_affinity_platform_constraints(self, k8s_service, mock_workload):
        mock_workload["spec"] = {
            "template": {
                "spec": {
                    "nodeSelector": {
                        "kubernetes.io/os": "linux",
                    },
                    "affinity": {
                        "nodeAffinity": {
                            "requiredDuringSchedulingIgnoredDuringExecution": {
                                "nodeSelectorTerms": [
                                    {
                                        "matchExpressions": [
                                            {
                                                "key": "kubernetes.io/arch",
                                                "operator": "In",
                                                "values": ["arm64"],
                                            }
                                        ]
                                    }
                                ]
                            }
                        }
                    },
                }
            }
        }
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        k8s_service.workload_provider.get_expiration.return_value = datetime.now(timezone.utc) + timedelta(hours=1)
        sandbox = k8s_service.get_sandbox("test-sandbox-123")

        assert sandbox.platform is not None
        assert sandbox.platform.os == "linux"
        assert sandbox.platform.arch == "arm64"

    def test_get_sandbox_returns_null_platform_for_default_scheduling(self, k8s_service, mock_workload):
        mock_workload["spec"] = {
            "template": {
                "spec": {
                    # no nodeSelector/nodeAffinity constraints
                }
            }
        }
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        k8s_service.workload_provider.get_expiration.return_value = datetime.now(timezone.utc) + timedelta(hours=1)
        sandbox = k8s_service.get_sandbox("test-sandbox-123")

        assert sandbox.platform is None

class TestDeleteSandbox:
    """delete_sandbox method tests"""
    
    def test_delete_existing_sandbox_succeeds(self, k8s_service, mock_workload):
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.delete_workload.return_value = None

        k8s_service.delete_sandbox("test-sandbox-id")
        
        k8s_service.workload_provider.delete_workload.assert_called_once_with(
            sandbox_id="test-sandbox-id",
            namespace=k8s_service.namespace
        )
    
    def test_delete_nonexistent_sandbox_raises_404(self, k8s_service):
        # Mock delete_workload to raise exception containing "not found"
        k8s_service.workload_provider.delete_workload.side_effect = Exception("Sandbox not found")

        with pytest.raises(HTTPException) as exc_info:
            k8s_service.delete_sandbox("nonexistent-id")
        
        assert exc_info.value.status_code == 404

class TestListSandboxes:
    """list_sandboxes method tests"""
    
    def test_list_all_sandboxes_succeeds(self, k8s_service, mock_workload):
        k8s_service.workload_provider.list_workloads.return_value = [mock_workload]
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        k8s_service.workload_provider.get_endpoint_info.return_value = "10.0.0.1:8080"
        k8s_service.workload_provider.get_expiration.return_value = datetime.now(timezone.utc) + timedelta(hours=1)
        
        from opensandbox_server.api.schema import PaginationRequest
        request = ListSandboxesRequest(pagination=PaginationRequest(page=1, page_size=20))
        response = k8s_service.list_sandboxes(request)
        
        # Sandbox in items uses 'id' field
        assert len(response.items) == 1
        assert response.items[0].id == "test-sandbox-123"
        assert response.pagination.total_items == 1
    
    def test_list_sandboxes_with_pagination(self, k8s_service, mock_workload):
        # Create multiple mock workloads using mock_workload as template
        workloads = []
        for i in range(10):
            workload = {
                "metadata": {
                    "name": f"sandbox-{i}",
                    "uid": f"uid-{i}",
                    "labels": {
                        "opensandbox.io/id": f"sandbox-{i}",
                    },
                    "annotations": mock_workload["metadata"]["annotations"].copy(),
                    "creationTimestamp": datetime.now(timezone.utc).isoformat(),
                },
                "spec": {},
                "status": {},
            }
            workloads.append(workload)
        
        k8s_service.workload_provider.list_workloads.return_value = workloads
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        k8s_service.workload_provider.get_endpoint_info.return_value = "10.0.0.1:8080"
        k8s_service.workload_provider.get_expiration.return_value = datetime.now(timezone.utc) + timedelta(hours=1)
        
        from opensandbox_server.api.schema import PaginationRequest
        request = ListSandboxesRequest(pagination=PaginationRequest(page=1, page_size=5))
        response = k8s_service.list_sandboxes(request)
        
        assert len(response.items) == 5
        assert response.pagination.page == 1
        assert response.pagination.page_size == 5
        assert response.pagination.total_items == 10
        assert response.pagination.total_pages == 2
    
    def test_list_sandboxes_sorted_by_creation_time(self, k8s_service, mock_workload):
        # Create workloads with different creation times
        base_time = datetime.now(timezone.utc)
        workloads = []
        
        # Create sandboxes with specific creation times
        # We'll create them in random order to verify sorting works
        creation_times = [
            base_time - timedelta(hours=5),  # Oldest
            base_time - timedelta(hours=2),
            base_time - timedelta(hours=1),
            base_time - timedelta(minutes=30),
            base_time,  # Newest
        ]
        
        for i, created_at in enumerate(creation_times):
            workload = {
                "metadata": {
                    "name": f"sandbox-{i}",
                    "uid": f"uid-{i}",
                    "labels": {
                        "opensandbox.io/id": f"sandbox-{i}",
                    },
                    "annotations": mock_workload["metadata"]["annotations"].copy(),
                    "creationTimestamp": created_at.isoformat(),
                },
                "spec": {},
                "status": {},
            }
            workloads.append(workload)
        
        k8s_service.workload_provider.list_workloads.return_value = workloads
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        k8s_service.workload_provider.get_endpoint_info.return_value = "10.0.0.1:8080"
        k8s_service.workload_provider.get_expiration.return_value = datetime.now(timezone.utc) + timedelta(hours=1)
        
        from opensandbox_server.api.schema import PaginationRequest
        request = ListSandboxesRequest(pagination=PaginationRequest(page=1, page_size=10))
        response = k8s_service.list_sandboxes(request)
        
        # Verify all items are returned
        assert len(response.items) == 5
        
        # Verify they are sorted by creation time (newest first)
        # The order should be: index 4 (newest), 3, 2, 1, 0 (oldest)
        assert response.items[0].id == "sandbox-4"  # Newest
        assert response.items[1].id == "sandbox-3"
        assert response.items[2].id == "sandbox-2"
        assert response.items[3].id == "sandbox-1"
        assert response.items[4].id == "sandbox-0"  # Oldest
        
        # Also verify the creation times are in descending order
        for i in range(len(response.items) - 1):
            assert response.items[i].created_at >= response.items[i + 1].created_at

    def test_list_sandboxes_returns_null_platform_for_default_scheduling(self, k8s_service):
        workloads = [
            {
                "metadata": {
                    "name": "sandbox-1",
                    "uid": "uid-1",
                    "labels": {"opensandbox.io/id": "sandbox-1"},
                    "annotations": {},
                    "creationTimestamp": datetime.now(timezone.utc).isoformat(),
                },
                "spec": {
                    "template": {
                        "spec": {
                            # Default scheduling: no nodeSelector/nodeAffinity constraints.
                        }
                    }
                },
                "status": {},
            }
        ]
        k8s_service.workload_provider.list_workloads.return_value = workloads
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": "",
            "message": "Running",
            "last_transition_at": datetime.now(timezone.utc),
        }
        k8s_service.workload_provider.get_expiration.return_value = datetime.now(timezone.utc) + timedelta(hours=1)
        from opensandbox_server.api.schema import PaginationRequest
        request = ListSandboxesRequest(pagination=PaginationRequest(page=1, page_size=10))
        response = k8s_service.list_sandboxes(request)

        assert len(response.items) == 1
        assert response.items[0].platform is None

class TestRenewExpiration:
    """renew_sandbox_expiration method tests"""
    
    def test_renew_expiration_succeeds(self, k8s_service, mock_workload):
        new_expiration = datetime.now(timezone.utc) + timedelta(hours=2)
        
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        k8s_service.workload_provider.update_expiration.return_value = None
        k8s_service.workload_provider.get_expiration.return_value = new_expiration
        
        from opensandbox_server.api.schema import RenewSandboxExpirationRequest
        request = RenewSandboxExpirationRequest(expires_at=new_expiration)
        
        response = k8s_service.renew_expiration("test-sandbox-id", request)
        
        assert response.expires_at == new_expiration
        k8s_service.workload_provider.update_expiration.assert_called_once_with(
            sandbox_id="test-sandbox-id",
            namespace=k8s_service.namespace,
            expires_at=new_expiration
        )
    
    def test_renew_with_past_time_raises_error(self, k8s_service, mock_workload):
        past_time = datetime.now(timezone.utc) - timedelta(hours=1)
        
        k8s_service.workload_provider.get_workload.return_value = mock_workload
        
        from opensandbox_server.api.schema import RenewSandboxExpirationRequest
        request = RenewSandboxExpirationRequest(expires_at=past_time)
        
        with pytest.raises(HTTPException) as exc_info:
            k8s_service.renew_expiration("test-sandbox-id", request)
        
        assert exc_info.value.status_code == 400

    def test_renew_returns_409_when_sandbox_has_no_expiration(self, k8s_service):
        """Renew is rejected with 409 when sandbox has no TTL (manual cleanup)."""
        k8s_service.workload_provider.get_workload.return_value = MagicMock()
        k8s_service.workload_provider.get_expiration.return_value = None
        from opensandbox_server.api.schema import RenewSandboxExpirationRequest
        request = RenewSandboxExpirationRequest(
            expires_at=datetime.now(timezone.utc) + timedelta(hours=1)
        )

        with pytest.raises(HTTPException) as exc_info:
            k8s_service.renew_expiration("no-ttl-sandbox", request)

        assert exc_info.value.status_code == 409
        assert "does not have automatic expiration" in exc_info.value.detail["message"]
        k8s_service.workload_provider.update_expiration.assert_not_called()


class TestSignedEndpoint:
    """Test signed route token generation in get_endpoint."""

    BASE64_SECRET = "bXktdGVzdC1zZWNyZXQ="  # "my-test-secret"

    def _setup_gateway_with_secure_access(self, k8s_service, route_mode="wildcard"):
        """Helper to configure ingress gateway with secure_access on the service."""
        address = "*.sandbox.example.com" if route_mode == "wildcard" else "gateway.sandbox.example.com"
        k8s_service.ingress_config = IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address=address,
                route=GatewayRouteModeConfig(mode=route_mode),
            ),
            secure_access=SecureAccessConfig(
                active_key="a",
                keys=[
                    SecureAccessKey(key_id="a", key=self.BASE64_SECRET),
                ],
            ),
        )

    def test_signed_endpoint_embeds_token_in_url(self, k8s_service):
        self._setup_gateway_with_secure_access(k8s_service)
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {"annotations": {}},
        }

        endpoint = k8s_service.get_endpoint("sbx-001", 8080, expires=2000000000)

        assert endpoint.endpoint.startswith("sbx-001-8080-")
        assert endpoint.endpoint.endswith(".sandbox.example.com")
        # The signature should be embedded in the endpoint URL (right-split for hyphenated sandbox_id)
        parts = endpoint.endpoint.split(".")[0].rsplit("-", 3)
        assert len(parts) == 4  # sandbox_id-port-b36-signature

    def test_signed_endpoint_omits_secure_access_header(self, k8s_service):
        """Signed endpoint must NOT include the static SecureAccessToken header."""
        self._setup_gateway_with_secure_access(k8s_service)
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {
                "annotations": {
                    SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY: "static-token",
                }
            },
        }

        endpoint = k8s_service.get_endpoint("sbx-001", 8080, expires=2000000000)

        if endpoint.headers:
            assert OPEN_SANDBOX_SECURE_ACCESS_HEADER not in endpoint.headers, (
                "Signed endpoint should not carry the static SecureAccessToken header"
            )

    def test_unsigned_endpoint_attaches_secure_access_header(self, k8s_service):
        """Unsigned endpoint must include the static SecureAccessToken header."""
        self._setup_gateway_with_secure_access(k8s_service)
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {
                "annotations": {
                    SANDBOX_SECURE_ACCESS_TOKEN_METADATA_KEY: "static-token",
                }
            },
        }
        k8s_service.workload_provider.get_endpoint_info.return_value = Endpoint(
            endpoint="sbx-001-8080.sandbox.example.com",
        )

        endpoint = k8s_service.get_endpoint("sbx-001", 8080)

        assert endpoint.headers is not None
        assert endpoint.headers[OPEN_SANDBOX_SECURE_ACCESS_HEADER] == "static-token"

    def test_signed_endpoint_header_mode(self, k8s_service):
        self._setup_gateway_with_secure_access(k8s_service, route_mode="header")
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {"annotations": {}},
        }

        endpoint = k8s_service.get_endpoint("sbx-001", 8080, expires=2000000000)

        assert endpoint.endpoint == "gateway.sandbox.example.com"
        assert endpoint.headers is not None
        ingress_val = endpoint.headers.get("OpenSandbox-Ingress-To", "")
        # Header value should contain the signed route: {sid}-{port}-{b36}-{sig}
        parts = ingress_val.rsplit("-", 3)
        assert len(parts) == 4

    def test_signed_endpoint_uri_mode(self, k8s_service):
        self._setup_gateway_with_secure_access(k8s_service, route_mode="uri")
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {"annotations": {}},
        }

        endpoint = k8s_service.get_endpoint("sbx-001", 8080, expires=2000000000)

        # URI mode: endpoint is {addr}/{sid}/{port}/{b36}/{sig}
        assert "x2qxvk" in endpoint.endpoint
        assert "0ff8cd39a" in endpoint.endpoint

    def test_expires_negative_rejected(self, k8s_service):
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {"annotations": {}},
        }

        with pytest.raises(HTTPException) as exc:
            k8s_service.get_endpoint("sbx-001", 8080, expires=-1)

        assert exc.value.status_code == 400

    def test_expires_in_past_rejected(self, k8s_service):
        """A timestamp in the past must be rejected."""
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {"annotations": {}},
        }

        with pytest.raises(HTTPException) as exc:
            k8s_service.get_endpoint("sbx-001", 8080, expires=1000000)

        assert exc.value.status_code == 400
        assert "must be greater than current time" in exc.value.detail["message"]

    def test_expires_without_gateway_rejected(self, k8s_service):
        """No ingress config at all."""
        k8s_service.ingress_config = None
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {"annotations": {}},
        }

        with pytest.raises(HTTPException) as exc:
            k8s_service.get_endpoint("sbx-001", 8080, expires=2000000000)

        assert exc.value.status_code == 400
        assert "gateway" in exc.value.detail["message"].lower()

    def test_expires_without_secure_access_keys_rejected(self, k8s_service):
        """Gateway configured but no secure_access keys block."""
        k8s_service.ingress_config = IngressConfig(
            mode="gateway",
            gateway=GatewayConfig(
                address="*.example.com",
                route=GatewayRouteModeConfig(mode="wildcard"),
            ),
        )
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {"annotations": {}},
        }

        with pytest.raises(HTTPException) as exc:
            k8s_service.get_endpoint("sbx-001", 8080, expires=2000000000)

        assert exc.value.status_code == 400
        assert "secure_access" in exc.value.detail["message"].lower()

    def test_unsigned_endpoint_no_expires(self, k8s_service):
        """Without expires, the unsigned endpoint should be returned."""
        self._setup_gateway_with_secure_access(k8s_service)
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {"annotations": {}},
        }
        k8s_service.workload_provider.get_endpoint_info.return_value = Endpoint(
            endpoint="sbx-001-8080.sandbox.example.com",
        )

        endpoint = k8s_service.get_endpoint("sbx-001", 8080)

        assert endpoint.endpoint == "sbx-001-8080.sandbox.example.com"

    def test_signed_endpoint_different_expires_produces_different_endpoints(self, k8s_service):
        self._setup_gateway_with_secure_access(k8s_service)
        k8s_service.workload_provider.get_workload.return_value = {
            "metadata": {"annotations": {}},
        }

        ep1 = k8s_service.get_endpoint("sbx-001", 8080, expires=2000000000)
        ep2 = k8s_service.get_endpoint("sbx-001", 8080, expires=2000000500)

        assert ep1.endpoint != ep2.endpoint


class TestPatchSandboxMetadata:
    """Verify patch_sandbox_metadata builds the JSON merge-patch body correctly
    and uses the API server's PATCH response (not a cache-prone re-fetch)."""

    @staticmethod
    def _workload(labels: dict) -> dict:
        return {
            "metadata": {
                "name": "sandbox-sbx-001",
                "labels": dict(labels),
                "creationTimestamp": datetime(2026, 1, 1, tzinfo=timezone.utc),
            },
            "spec": {},
            "status": {"conditions": []},
        }

    @staticmethod
    def _stub_provider_status(k8s_service) -> None:
        k8s_service.workload_provider.get_status.return_value = {
            "state": "Running",
            "reason": None,
            "message": None,
            "last_transition_at": None,
        }
        k8s_service.workload_provider.get_expiration.return_value = None

    def test_patch_body_sends_null_for_deleted_keys(self, k8s_service):
        initial = {"opensandbox.io/id": "sbx-001", "team": "infra", "env": "dev"}
        patched = {"opensandbox.io/id": "sbx-001", "env": "stage"}

        k8s_service.workload_provider.get_workload.return_value = self._workload(initial)
        k8s_service.workload_provider.patch_labels.return_value = self._workload(patched)
        self._stub_provider_status(k8s_service)

        k8s_service.patch_sandbox_metadata("sbx-001", {"env": "stage", "team": None})

        k8s_service.workload_provider.patch_labels.assert_called_once()
        body_labels = k8s_service.workload_provider.patch_labels.call_args.kwargs["labels"]
        assert body_labels["env"] == "stage"
        assert body_labels["team"] is None
        assert body_labels["opensandbox.io/id"] == "sbx-001"

    def test_returns_sandbox_from_patch_response(self, k8s_service):
        """The PATCH response is authoritative; re-reading via get_workload
        could hit a stale informer cache."""
        initial = {"opensandbox.io/id": "sbx-001", "env": "dev"}
        patched = {"opensandbox.io/id": "sbx-001", "env": "stage"}

        k8s_service.workload_provider.get_workload.return_value = self._workload(initial)
        k8s_service.workload_provider.patch_labels.return_value = self._workload(patched)
        self._stub_provider_status(k8s_service)

        sandbox = k8s_service.patch_sandbox_metadata("sbx-001", {"env": "stage"})

        assert sandbox.metadata == {"env": "stage"}
        # Pre-patch read only; no second get_workload after patch_labels.
        assert k8s_service.workload_provider.get_workload.call_count == 1
