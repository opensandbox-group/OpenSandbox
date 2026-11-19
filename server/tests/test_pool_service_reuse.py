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

"""The pool routes share one PoolService instead of building a K8sClient per request."""

import threading
import time
from concurrent.futures import ThreadPoolExecutor
from types import SimpleNamespace

import pytest
from fastapi.exceptions import HTTPException

from opensandbox_server.api import pool
from opensandbox_server.config import AppConfig
from opensandbox_server.services.k8s import client as k8s_client_module
from opensandbox_server.services.k8s.batchsandbox_provider import BatchSandboxProvider


@pytest.fixture
def k8s_clients(monkeypatch):
    """Count K8sClient constructions; each one reloads the kubeconfig."""
    built = []
    lock = threading.Lock()

    class FakeK8sClient:
        def __init__(self, k8s_config):
            # Loading a kubeconfig takes a while; widen the window in which
            # unsynchronized first requests would each build a client.
            time.sleep(0.05)
            with lock:
                built.append(self)

        def get_custom_objects_api(self):
            return object()

    monkeypatch.setattr(k8s_client_module, "K8sClient", FakeK8sClient)
    monkeypatch.setattr(pool, "_service", None)
    monkeypatch.setattr(pool, "_pool_unsupported", False)
    monkeypatch.setattr(
        pool,
        "get_config",
        lambda: SimpleNamespace(
            runtime=SimpleNamespace(type="kubernetes"),
            kubernetes=SimpleNamespace(namespace="sandboxes"),
        ),
    )
    return built


def test_pool_service_is_built_once(k8s_clients):
    first = pool._get_pool_service()
    second = pool._get_pool_service()

    assert first is second
    assert len(k8s_clients) == 1


def test_concurrent_first_requests_share_one_pool_service(k8s_clients):
    # Pool routes are sync handlers, so FastAPI runs them on a thread pool.
    with ThreadPoolExecutor(max_workers=8) as executor:
        services = list(executor.map(lambda _: pool._get_pool_service(), range(32)))

    assert len({id(service) for service in services}) == 1
    assert len(k8s_clients) == 1


def test_non_kubernetes_runtime_still_returns_501(monkeypatch, k8s_clients):
    monkeypatch.setattr(
        pool,
        "get_config",
        lambda: SimpleNamespace(runtime=SimpleNamespace(type="docker"), kubernetes=None),
    )

    with pytest.raises(HTTPException) as exc:
        pool._get_pool_service()

    assert exc.value.status_code == 501
    assert k8s_clients == []


def test_agent_sandbox_provider_returns_501_without_building_client(monkeypatch, k8s_clients):
    monkeypatch.setattr(
        pool,
        "get_config",
        lambda: SimpleNamespace(
            runtime=SimpleNamespace(type="kubernetes"),
            kubernetes=SimpleNamespace(namespace="sandboxes", workload_provider="agent-sandbox"),
        ),
    )

    with pytest.raises(HTTPException) as exc:
        pool._get_pool_service()

    assert exc.value.status_code == 501
    assert exc.value.detail["code"] == "KUBERNETES::POOL_NOT_SUPPORTED"
    assert k8s_clients == []


def test_provider_returning_none_builds_one_client(monkeypatch, k8s_clients):
    monkeypatch.setattr(BatchSandboxProvider, "create_pool_service", lambda self, namespace: None)

    def request_pool_service(_):
        with pytest.raises(HTTPException) as exc:
            pool._get_pool_service()
        assert exc.value.status_code == 501
        assert isinstance(exc.value.detail, dict)
        assert exc.value.detail.get("code") == "KUBERNETES::POOL_NOT_SUPPORTED"

    with ThreadPoolExecutor(max_workers=4) as executor:
        list(executor.map(request_pool_service, range(8)))
    assert len(k8s_clients) == 1


@pytest.mark.parametrize("template", [None, "[]", "spec: ["])
def test_invalid_provider_template_returns_503_and_can_be_retried(
    monkeypatch, k8s_clients, tmp_path, template
):
    path = tmp_path / "batchsandbox.yaml"
    if template is not None:
        path.write_text(template)
    config = AppConfig.model_validate({
        "runtime": {"type": "kubernetes", "execd_image": "test"},
        "kubernetes": {"batchsandbox_template_file": str(path)},
    })
    monkeypatch.setattr(pool, "get_config", lambda: config)

    with pytest.raises(HTTPException) as exc:
        pool._get_pool_service()
    assert exc.value.status_code == 503
    assert isinstance(exc.value.detail, dict)
    assert exc.value.detail.get("code") == "KUBERNETES::INITIALIZATION_ERROR"

    path.write_text("{}")
    assert pool._get_pool_service() is not None
