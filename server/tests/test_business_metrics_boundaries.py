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

"""Independent failure-injection probes for PR #1770 (boundaries)."""

import asyncio
from types import SimpleNamespace
from unittest.mock import patch

import httpx
import pytest
from opentelemetry.sdk.metrics import MeterProvider
from opentelemetry.sdk.metrics.export import (
    HistogramDataPoint,
    InMemoryMetricReader,
    NumberDataPoint,
)

from opensandbox_server.api.schema import CreateSandboxRequest
from opensandbox_server.integrations.otel import metrics as telemetry
from opensandbox_server.services.docker.docker_service import DockerSandboxService
from opensandbox_server.tenants.context import get_current_tenant, set_current_tenant
from opensandbox_server.tenants.http_provider import (
    HTTPTenantProvider,
    HTTPTenantProviderConfig,
)


def points(reader, name):
    data = reader.get_metrics_data()
    if data is None:
        return []
    return [
        point
        for resource in data.resource_metrics
        for scope in resource.scope_metrics
        for metric in scope.metrics
        if metric.name == name
        for point in metric.data.data_points
    ]


@pytest.mark.asyncio
@pytest.mark.parametrize("cancel", [False, True], ids=["runtime-error", "cancelled"])
async def test_docker_create_interruption_is_counted(cancel):
    """Enter the actual decorated service, interrupt at its first async dependency."""
    entered = asyncio.Event()
    release = asyncio.Event()

    async def resolve_image(request):
        entered.set()
        await release.wait()
        raise RuntimeError("injected image resolution failure")

    reader = InMemoryMetricReader()
    provider = MeterProvider(metric_readers=[reader])
    instruments = telemetry._business_instruments_from_provider(provider)
    # No initialized Docker client is needed: the resolver is reached before
    # any self attributes or Docker calls. The method and wrapper are real.
    service = object.__new__(DockerSandboxService)
    request = CreateSandboxRequest.model_validate(
        {
            "image": {"uri": "python:3.11"},
            "timeout": 120,
            "resourceLimits": {"cpu": "500m", "memory": "512Mi"},
            "entrypoint": ["python"],
        }
    )
    task = None
    try:
        with (
            patch.object(telemetry, "_business_instruments", instruments),
            patch.object(telemetry, "_configured_runtime_label", return_value="docker"),
            patch(
                "opensandbox_server.services.docker.docker_service."
                "resolve_sandbox_image_from_request",
                resolve_image,
            ),
        ):
            task = asyncio.create_task(service.create_sandbox(request))
            await asyncio.wait_for(entered.wait(), 2)
            if cancel:
                task.cancel()
                with pytest.raises(asyncio.CancelledError):
                    await task
            else:
                release.set()
                with pytest.raises(RuntimeError, match="injected"):
                    await task
            counts = points(reader, "server.sandbox.create.count")
            durations = points(reader, "server.sandbox.create.duration")
            assert len(counts) == 1
            assert len(durations) == 1
            assert isinstance(counts[0], NumberDataPoint)
            assert counts[0].value == 1
            assert isinstance(durations[0], HistogramDataPoint)
            assert durations[0].count == 1
            assert counts[0].attributes is not None
            assert counts[0].attributes["result"] == "error"
    finally:
        if task is not None and not task.done():
            task.cancel()
            await asyncio.gather(task, return_exceptions=True)
        provider.shutdown()


def test_http_tenant_gauge_current_coverage():
    """Characterize coverage; cached HTTP tenants are currently omitted."""
    tenant_provider = HTTPTenantProvider(
        HTTPTenantProviderConfig(endpoint="https://tenant.invalid/lookup")
    )
    transport = httpx.MockTransport(
        lambda request: httpx.Response(200, json={"namespace": "tenant-a", "ttl": 60})
    )
    visited = []

    def list_sandboxes(request):
        tenant = get_current_tenant()
        namespace = tenant.namespace if tenant else "default"
        visited.append(namespace)
        items = [SimpleNamespace(id="tenant-sandbox", status=SimpleNamespace(state="Running"))]
        return SimpleNamespace(
            items=items if namespace == "tenant-a" else [],
            pagination=SimpleNamespace(has_next_page=False),
        )

    import opensandbox_server.api.lifecycle as lifecycle
    import opensandbox_server.main as main

    original_tenant = get_current_tenant()
    set_current_tenant(None)
    reader = InMemoryMetricReader()
    provider = MeterProvider(metric_readers=[reader])
    try:
        with (
            httpx.Client(transport=transport) as client,
            patch.object(tenant_provider, "_client", client),
        ):
            tenant = tenant_provider.lookup("synthetic-test-key")
            assert tenant is not None
            assert tenant.namespace == "tenant-a"
            assert len(tenant_provider.list_tenants()) == 1
            assert not tenant_provider.supports_enumeration
            with (
                patch.object(main, "tenant_provider", tenant_provider),
                patch.object(
                    lifecycle, "sandbox_service", SimpleNamespace(list_sandboxes=list_sandboxes)
                ),
                patch.object(telemetry, "_configured_runtime_label", return_value="kubernetes"),
            ):
                telemetry._register_sandbox_active_gauge(provider)
                assert points(reader, "server.sandbox.active") == []
                assert visited == ["default"]
    finally:
        provider.shutdown()
        set_current_tenant(original_tenant)
