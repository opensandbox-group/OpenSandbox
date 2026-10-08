# Copyright 2026 The OpenSandbox Authors
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

import asyncio
from datetime import datetime, timezone
from types import SimpleNamespace
from unittest.mock import Mock

from fastapi import HTTPException
import httpx
import pytest

from opensandbox_server.api.fork_schema import ForkSandboxRequest
from opensandbox_server.api.schema import Sandbox, SandboxStatus
from opensandbox_server.services import fork_config
from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider


def test_live_policy_envelope_is_inherited_without_widening_access(monkeypatch):
    service = Mock()
    # A real backend object, not an auto-vivifying Mock attribute tree.
    backend = SimpleNamespace(docker_client=Mock(),
        app_config=SimpleNamespace(server=SimpleNamespace(max_sandbox_timeout_seconds=3600)),
        _ensure_secure_access_support=Mock(), _ensure_network_policy_support=Mock(), _prepare_resource_limits=Mock())
    service._kubernetes = backend
    service.get_sandbox.return_value = Sandbox(id="source", status=SandboxStatus(state="Running"),
        createdAt=datetime.now(timezone.utc))
    backend.get_endpoint = Mock(return_value=SimpleNamespace(endpoint="sandbox.test:18080", headers={"token": "source-token"}))
    monkeypatch.setattr(fork_config, "export_docker", lambda *_: ({"env": {}, "resourceLimits": {"cpu": "1"}}, True))
    client_type = httpx.AsyncClient
    transport = httpx.MockTransport(lambda request: httpx.Response(200, json={
        "status": "ok", "policy": {"defaultAction": "deny", "egress": [{"action": "allow", "target": "example.org"}]}}))
    monkeypatch.setattr(fork_config.httpx, "AsyncClient", lambda **kwargs: client_type(transport=transport, **kwargs))
    config = asyncio.run(fork_config.build_fork_config(service, "source", ForkSandboxRequest(timeout=1800)))
    assert config["networkPolicy"] == {"defaultAction": "deny", "egress": [{"action": "allow", "target": "example.org"}]}
    assert config["entrypoint"] == ["tail", "-f", "/dev/null"]


def kubernetes_source(user):
    provider = Mock(spec=BatchSandboxProvider)
    provider.get_workload.return_value = {"metadata": {}, "spec": {"replicas": 1, "template": {"spec": {"containers": []}}}}
    service = SimpleNamespace(workload_provider=provider, k8s_client=Mock(), _resolve_namespace_for_lookup=lambda _: "default")
    service.k8s_client.list_pods.return_value = [{"status": {"phase": "Running"}, "spec": {"containers": [user]}}]
    return service


def test_kubernetes_export_uses_live_pod_env_and_resources():
    user = {"name": "sandbox", "env": [{"name": "USER_SETTING", "value": "live"}, {"name": "OPENSANDBOX_ID", "value": "source"}],
            "resources": {"limits": {"cpu": "2", "memory": "1Gi"}, "requests": {"cpu": "500m"}}}
    config, egress = fork_config.export_kubernetes(kubernetes_source(user), "source")
    assert config["env"] == {"USER_SETTING": "live"}
    assert config["resourceLimits"]["cpu"] == "2"
    assert config["resourceRequests"] == {"cpu": "500m"}
    assert not egress


def test_kubernetes_admission_injected_user_mount_is_rejected():
    user = {"name": "sandbox", "volumeMounts": [{"name": "admission-pvc", "mountPath": "/data"}]}
    with pytest.raises(HTTPException) as caught:
        fork_config.export_kubernetes(kubernetes_source(user), "source")
    assert caught.value.detail["code"] == "FORK::VOLUMES_UNSUPPORTED"


def test_kubernetes_custom_template_is_rejected():
    service = kubernetes_source({"name": "sandbox"})
    service.app_config = SimpleNamespace(kubernetes=SimpleNamespace(batchsandbox_template_file="custom.yaml"))
    with pytest.raises(HTTPException) as caught:
        fork_config.export_kubernetes(service, "source")
    assert caught.value.detail["code"] == "FORK::UNSUPPORTED_RUNTIME"
