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

from __future__ import annotations

from datetime import datetime, timezone
from unittest.mock import MagicMock, patch

import pytest

from opensandbox_server.api.schema import CreateSandboxRequest
from opensandbox_server.config import (
    AgentSubstrateRuntimeConfig,
    AgentSubstrateTemplateConfig,
    AppConfig,
    KubernetesRuntimeConfig,
    RuntimeConfig,
)
from opensandbox_server.services.constants import SANDBOX_ID_LABEL
from opensandbox_server.services.k8s.agent_substrate_client import AgentSubstrateClient
from opensandbox_server.services.k8s.agent_substrate_proto import ateapi_pb2
from opensandbox_server.services.k8s.agent_substrate_provider import (
    AgentSubstrateProvider,
)
from opensandbox_server.services.k8s.client import K8sClient
from opensandbox_server.services.k8s.kubernetes_service import KubernetesSandboxService
from opensandbox_server.well_known_extensions import AGENT_SUBSTRATE_TEMPLATE_KEY


class FakeControlStub:
    def __init__(self):
        self.calls = []
        self.actor = None

    def CreateActor(self, request, timeout):
        self.calls.append(("CreateActor", request, timeout))
        self.actor = ateapi_pb2.Actor(
            metadata=ateapi_pb2.ResourceMetadata(
                atespace=request.actor.metadata.atespace,
                name=request.actor.metadata.name,
                uid="actor-uid",
            ),
            actor_template_namespace=request.actor.actor_template_namespace,
            actor_template_name=request.actor.actor_template_name,
            status=ateapi_pb2.Actor.STATUS_SUSPENDED,
        )
        return self.actor

    def ResumeActor(self, request, timeout):
        self.calls.append(("ResumeActor", request, timeout))
        self.actor.status = ateapi_pb2.Actor.STATUS_RUNNING
        self.actor.ateom_pod_ip = "10.0.0.12"
        return ateapi_pb2.ResumeActorResponse(actor=self.actor)

    def GetActor(self, request, timeout):
        self.calls.append(("GetActor", request, timeout))
        return self.actor

    def ListActors(self, request, timeout):
        self.calls.append(("ListActors", request, timeout))
        return ateapi_pb2.ListActorsResponse(actors=[self.actor])

    def SuspendActor(self, request, timeout):
        self.calls.append(("SuspendActor", request, timeout))
        self.actor.status = ateapi_pb2.Actor.STATUS_SUSPENDED
        self.actor.ateom_pod_ip = ""
        return ateapi_pb2.SuspendActorResponse(actor=self.actor)

    def PauseActor(self, request, timeout):
        raise AssertionError("OpenSandbox pause must not call node-local PauseActor")

    def DeleteActor(self, request, timeout):
        self.calls.append(("DeleteActor", request, timeout))
        return self.actor


def _app_config() -> AppConfig:
    return AppConfig(
        runtime=RuntimeConfig(
            type="kubernetes",
            execd_image="unused-for-agent-substrate",
        ),
        kubernetes=KubernetesRuntimeConfig(
            namespace="opensandbox-system",
            workload_provider="agent-substrate",
        ),
        agent_substrate=AgentSubstrateRuntimeConfig(
            api_endpoint="api.ate-system.svc:443",
            router_endpoint="http://atenet-router.ate-system.svc:80",
            default_atespace="opensandbox",
            templates=[
                AgentSubstrateTemplateConfig(
                    key="python",
                    namespace="opensandbox-templates",
                    name="python-execd",
                )
            ],
        ),
    )


def _provider():
    config = _app_config()
    stub = FakeControlStub()
    client = AgentSubstrateClient(config.agent_substrate, stub=stub)
    k8s_client = MagicMock(spec=K8sClient)
    provider = AgentSubstrateProvider(
        k8s_client,
        app_config=config,
        client=client,
    )
    return provider, stub, k8s_client


def _create(provider: AgentSubstrateProvider):
    return provider.create_workload(
        sandbox_id="01234567-89ab-cdef-0123-456789abcdef",
        namespace="opensandbox-system",
        image_spec=None,  # type: ignore[arg-type]
        entrypoint=None,  # type: ignore[arg-type]
        env={},
        resource_limits={},
        labels={SANDBOX_ID_LABEL: "01234567-89ab-cdef-0123-456789abcdef"},
        expires_at=datetime(2099, 7, 15, tzinfo=timezone.utc),
        execd_image="unused-for-agent-substrate",
        extensions={AGENT_SUBSTRATE_TEMPLATE_KEY: "python"},
    )


def test_create_uses_pinned_actor_requests_without_kubernetes_mutation():
    provider, stub, k8s_client = _provider()

    result = _create(provider)

    assert result == {
        "name": "osb-01234567-89ab-cdef-0123-456789abcdef",
        "uid": "actor-uid",
        "apiVersion": "ateapi/v1",
        "kind": "Actor",
    }
    assert [call[0] for call in stub.calls] == ["CreateActor", "ResumeActor"]

    create_request = stub.calls[0][1]
    assert create_request.actor.metadata.atespace == "opensandbox"
    assert create_request.actor.metadata.name == result["name"]
    assert create_request.actor.actor_template_namespace == "opensandbox-templates"
    assert create_request.actor.actor_template_name == "python-execd"

    resume_request = stub.calls[1][1]
    assert resume_request.actor.atespace == "opensandbox"
    assert resume_request.actor.name == result["name"]

    k8s_client.create_custom_object.assert_not_called()
    k8s_client.patch_custom_object.assert_not_called()
    k8s_client.delete_custom_object.assert_not_called()
    k8s_client.create_pvc.assert_not_called()


def test_pause_uses_suspend_and_endpoint_routes_through_atenet():
    provider, stub, _ = _provider()
    _create(provider)

    provider.pause_sandbox(
        "01234567-89ab-cdef-0123-456789abcdef",
        "opensandbox-system",
    )
    workload = provider.get_workload(
        "01234567-89ab-cdef-0123-456789abcdef",
        "opensandbox-system",
    )

    assert workload is not None
    assert provider.get_status(workload)["state"] == "Paused"
    assert "SuspendActor" in [call[0] for call in stub.calls]
    endpoint = provider.get_endpoint_info(workload, 44772, "ignored")
    assert endpoint is not None
    assert endpoint.endpoint == "http://atenet-router.ate-system.svc:80"
    assert endpoint.headers == {
        "Host": (
            "osb-01234567-89ab-cdef-0123-456789abcdef."
            "opensandbox.actors.resources.substrate.ate.dev"
        )
    }


def test_create_failure_does_not_delete_an_unowned_actor():
    provider, stub, _ = _provider()
    stub.CreateActor = MagicMock(side_effect=RuntimeError("already exists"))
    stub.SuspendActor = MagicMock()
    stub.DeleteActor = MagicMock()

    with pytest.raises(RuntimeError, match="already exists"):
        _create(provider)

    stub.SuspendActor.assert_not_called()
    stub.DeleteActor.assert_not_called()
    assert provider.get_workload(
        "01234567-89ab-cdef-0123-456789abcdef",
        "opensandbox-system",
    ) is None


@pytest.mark.asyncio
async def test_kubernetes_service_runs_actor_lifecycle_without_kubernetes_mutation():
    config = _app_config()
    stub = FakeControlStub()
    client = AgentSubstrateClient(config.agent_substrate, stub=stub)
    k8s_client = MagicMock(spec=K8sClient)

    with patch(
        "opensandbox_server.services.k8s.kubernetes_service.K8sClient",
        return_value=k8s_client,
    ), patch(
        "opensandbox_server.services.k8s.agent_substrate_provider.AgentSubstrateClient",
        return_value=client,
    ):
        service = KubernetesSandboxService(config)

    request = CreateSandboxRequest(
        timeout=600,
        metadata={"team": "research"},
        extensions={AGENT_SUBSTRATE_TEMPLATE_KEY: "python"},
    )
    response = await service.create_sandbox(request)

    assert response.status.state == "Running"
    endpoint = service.get_endpoint(response.id, 44772)
    assert endpoint.endpoint == "http://atenet-router.ate-system.svc:80"
    assert endpoint.headers is not None
    assert endpoint.headers["Host"].endswith(
        ".opensandbox.actors.resources.substrate.ate.dev"
    )

    service.delete_sandbox(response.id)

    assert [call[0] for call in stub.calls].count("CreateActor") == 1
    assert [call[0] for call in stub.calls].count("ResumeActor") == 1
    assert [call[0] for call in stub.calls].count("SuspendActor") == 1
    assert [call[0] for call in stub.calls].count("DeleteActor") == 1
    k8s_client.create_custom_object.assert_not_called()
    k8s_client.patch_custom_object.assert_not_called()
    k8s_client.delete_custom_object.assert_not_called()
    k8s_client.create_pvc.assert_not_called()
    k8s_client.list_pvcs.assert_not_called()