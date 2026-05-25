#
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
#
from __future__ import annotations

import asyncio
from datetime import datetime, timedelta, timezone
from uuid import uuid4

import pytest

from opensandbox.config import ConnectionConfig
from opensandbox.constants import DEFAULT_EGRESS_PORT, DEFAULT_EXECD_PORT
from opensandbox.exceptions import SandboxReadyTimeoutException
from opensandbox.models.diagnostics import DiagnosticContent
from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule, SandboxEndpoint
from opensandbox.sandbox import Sandbox


class _SandboxServiceStub:
    def __init__(self) -> None:
        self.renew_calls: list[tuple[object, datetime]] = []
        self.endpoint_calls: list[tuple[object, int, bool]] = []

    async def renew_sandbox_expiration(self, sandbox_id, expires_at: datetime) -> None:
        self.renew_calls.append((sandbox_id, expires_at))

    async def get_sandbox_endpoint(self, sandbox_id, port: int, use_server_proxy: bool = False) -> SandboxEndpoint:
        self.endpoint_calls.append((sandbox_id, port, use_server_proxy))
        return SandboxEndpoint(endpoint=f"sbx.internal:{port}", headers={"X-Egress": "1"})


class _HealthServiceStub:
    def __init__(self, *, should_raise: bool = False) -> None:
        self.should_raise = should_raise
        self.ping_calls: list[object] = []

    async def ping(self, sandbox_id) -> bool:
        self.ping_calls.append(sandbox_id)
        if self.should_raise:
            raise RuntimeError("boom")
        return True


class _Noop:
    pass


class _EgressServiceStub:
    def __init__(self) -> None:
        self.patch_calls: list[list[NetworkRule]] = []

    async def get_policy(self) -> NetworkPolicy:
        return NetworkPolicy(
            defaultAction="deny",
            egress=[NetworkRule(action="allow", target="pypi.org")],
        )

    async def patch_rules(self, rules: list[NetworkRule]) -> None:
        self.patch_calls.append(rules)


class _DiagnosticsServiceStub:
    def __init__(self) -> None:
        self.calls: list[tuple[str, object, str | None]] = []

    async def get_logs(self, sandbox_id: str, scope: str) -> DiagnosticContent:
        self.calls.append(("logs", sandbox_id, scope))
        return DiagnosticContent(
            sandboxId=sandbox_id,
            kind="logs",
            scope=scope or "container",
            delivery="inline",
            contentType="text/plain; charset=utf-8",
            content="log line",
            truncated=False,
        )

    async def get_events(self, sandbox_id: str, scope: str) -> DiagnosticContent:
        self.calls.append(("events", sandbox_id, scope))
        return DiagnosticContent(
            sandboxId=sandbox_id,
            kind="events",
            scope=scope or "runtime",
            delivery="inline",
            contentType="text/plain; charset=utf-8",
            content="event line",
            truncated=False,
        )


def _make_sandbox(
    *,
    health_service,
    sandbox_service,
    diagnostics_service=None,
    custom_health_check=None,
    connection_config: ConnectionConfig | None = None,
) -> Sandbox:
    return Sandbox(
        sandbox_id=str(uuid4()),
        sandbox_service=sandbox_service,
        filesystem_service=_Noop(),
        command_service=_Noop(),
        health_service=health_service,
        metrics_service=_Noop(),
        egress_service=_EgressServiceStub(),
        diagnostics_service=diagnostics_service or _DiagnosticsServiceStub(),
        connection_config=connection_config or ConnectionConfig(),
        custom_health_check=custom_health_check,
    )


@pytest.mark.asyncio
async def test_is_healthy_uses_ping_and_swallows_ping_errors() -> None:
    sbx = _make_sandbox(
        health_service=_HealthServiceStub(should_raise=True),
        sandbox_service=_SandboxServiceStub(),
    )
    assert await sbx.is_healthy() is False


@pytest.mark.asyncio
async def test_check_ready_succeeds_after_retries_without_real_sleep(monkeypatch: pytest.MonkeyPatch) -> None:
    # Avoid actual sleeping even if polling_interval > 0.
    async def _no_sleep(_: float) -> None:
        return None

    monkeypatch.setattr("opensandbox.sandbox.asyncio.sleep", _no_sleep)

    calls = {"n": 0}

    async def _custom_health(_: Sandbox) -> bool:
        calls["n"] += 1
        return calls["n"] >= 3

    sbx = _make_sandbox(
        health_service=_HealthServiceStub(),
        sandbox_service=_SandboxServiceStub(),
        custom_health_check=_custom_health,
    )

    await sbx.check_ready(timeout=timedelta(seconds=1), polling_interval=timedelta(seconds=0.01))
    assert calls["n"] == 3


@pytest.mark.asyncio
async def test_check_ready_timeout_raises() -> None:
    async def _always_false(_: Sandbox) -> bool:
        return False

    sbx = _make_sandbox(
        health_service=_HealthServiceStub(),
        sandbox_service=_SandboxServiceStub(),
        custom_health_check=_always_false,
    )

    with pytest.raises(SandboxReadyTimeoutException):
        await sbx.check_ready(timeout=timedelta(seconds=0.01), polling_interval=timedelta(seconds=0))


@pytest.mark.asyncio
async def test_check_ready_timeout_message_includes_troubleshooting_hints() -> None:
    async def _always_false(_: Sandbox) -> bool:
        return False

    sbx = _make_sandbox(
        health_service=_HealthServiceStub(),
        sandbox_service=_SandboxServiceStub(),
        custom_health_check=_always_false,
        connection_config=ConnectionConfig(domain="10.0.0.1:8080", use_server_proxy=False),
    )

    with pytest.raises(SandboxReadyTimeoutException) as exc_info:
        await sbx.check_ready(timeout=timedelta(seconds=0.01), polling_interval=timedelta(seconds=0))

    message = str(exc_info.value)
    assert "ConnectionConfig(domain=10.0.0.1:8080, use_server_proxy=False)" in message
    assert "ConnectionConfig(use_server_proxy=True)" in message


@pytest.mark.asyncio
async def test_renew_passes_timezone_aware_utc_datetime() -> None:
    svc = _SandboxServiceStub()
    sbx = _make_sandbox(
        health_service=_HealthServiceStub(),
        sandbox_service=svc,
    )

    before = datetime.now(timezone.utc)
    await sbx.renew(timedelta(seconds=10))
    after = datetime.now(timezone.utc)

    assert len(svc.renew_calls) == 1
    _, expires_at = svc.renew_calls[0]
    assert expires_at.tzinfo is timezone.utc
    assert before <= expires_at <= after + timedelta(seconds=12)


@pytest.mark.asyncio
async def test_get_egress_policy_uses_injected_egress_service() -> None:
    sbx = _make_sandbox(
        health_service=_HealthServiceStub(),
        sandbox_service=_SandboxServiceStub(),
        connection_config=ConnectionConfig(use_server_proxy=True),
    )

    policy = await sbx.get_egress_policy()

    assert policy.default_action == "deny"
    assert policy.egress is not None
    assert policy.egress[0].target == "pypi.org"


@pytest.mark.asyncio
async def test_patch_egress_rules_uses_injected_egress_service(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    svc = _SandboxServiceStub()
    egress_service = _EgressServiceStub()

    sbx = Sandbox(
        sandbox_id=str(uuid4()),
        sandbox_service=svc,
        filesystem_service=_Noop(),
        command_service=_Noop(),
        health_service=_HealthServiceStub(),
        metrics_service=_Noop(),
        egress_service=egress_service,
        diagnostics_service=_DiagnosticsServiceStub(),
        connection_config=ConnectionConfig(use_server_proxy=False),
    )
    rules = [NetworkRule(action="allow", target="www.github.com")]

    await sbx.patch_egress_rules(rules)

    assert svc.endpoint_calls == []
    assert egress_service.patch_calls == [rules]


@pytest.mark.asyncio
async def test_get_diagnostics_uses_injected_diagnostics_service() -> None:
    diagnostics_service = _DiagnosticsServiceStub()
    sbx = _make_sandbox(
        health_service=_HealthServiceStub(),
        sandbox_service=_SandboxServiceStub(),
        diagnostics_service=diagnostics_service,
    )

    logs = await sbx.get_diagnostic_logs(scope="container")
    events = await sbx.diagnostics.get_events(sbx.id, scope="runtime")

    assert logs.kind == "logs"
    assert events.kind == "events"
    assert diagnostics_service.calls == [
        ("logs", sbx.id, "container"),
        ("events", sbx.id, "runtime"),
    ]


@pytest.mark.asyncio
async def test_create_resolves_egress_endpoint_and_builds_service(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    egress_service = _EgressServiceStub()
    factory_calls: list[SandboxEndpoint] = []

    class _CreateResponse:
        id = "sbx-created"

    class _SandboxServiceCreateStub:
        def __init__(self) -> None:
            self.endpoint_calls: list[tuple[str, int, bool]] = []

        async def create_sandbox(self, *_args, **_kwargs):
            return _CreateResponse()

        async def get_sandbox_endpoint(self, sandbox_id, port: int, use_server_proxy: bool = False) -> SandboxEndpoint:
            self.endpoint_calls.append((sandbox_id, port, use_server_proxy))
            return SandboxEndpoint(endpoint=f"sbx.internal:{port}", headers={"X-Port": str(port)})

        async def kill_sandbox(self, _sandbox_id: str) -> None:
            return None

    class _FactoryStub:
        def __init__(self, connection_config: ConnectionConfig) -> None:
            self.connection_config = connection_config

        def create_sandbox_service(self):
            return sandbox_service

        def create_filesystem_service(self, endpoint: SandboxEndpoint):
            return _Noop()

        def create_command_service(self, endpoint: SandboxEndpoint):
            return _Noop()

        def create_health_service(self, endpoint: SandboxEndpoint):
            return _Noop()

        def create_metrics_service(self, endpoint: SandboxEndpoint):
            return _Noop()

        def create_egress_service(self, endpoint: SandboxEndpoint) -> _EgressServiceStub:
            factory_calls.append(endpoint)
            return egress_service

        def create_diagnostics_service(self):
            return _DiagnosticsServiceStub()

    sandbox_service = _SandboxServiceCreateStub()
    monkeypatch.setattr("opensandbox.sandbox.AdapterFactory", _FactoryStub)

    async def _healthy(_sbx: Sandbox) -> bool:
        return True

    await Sandbox.create(
        "python:3.11",
        connection_config=ConnectionConfig(use_server_proxy=False),
        health_check=_healthy,
    )

    assert sandbox_service.endpoint_calls == [
        ("sbx-created", DEFAULT_EXECD_PORT, False),
        ("sbx-created", DEFAULT_EGRESS_PORT, False),
    ]
    assert len(factory_calls) == 1
    assert factory_calls == [
        SandboxEndpoint(
            endpoint=f"sbx.internal:{DEFAULT_EGRESS_PORT}",
            headers={"X-Port": str(DEFAULT_EGRESS_PORT)},
        )
    ]


@pytest.mark.asyncio
async def test_create_cancellation_cleans_up_created_sandbox(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class _CreateResponse:
        id = "sbx-created-before-cancel"

    class _SandboxServiceCreateStub:
        def __init__(self) -> None:
            self.created = asyncio.Event()
            self.killed: list[str] = []

        async def create_sandbox(self, *_args, **_kwargs):
            self.created.set()
            return _CreateResponse()

        async def get_sandbox_endpoint(
            self,
            sandbox_id: str,
            port: int,
            use_server_proxy: bool = False,
        ) -> SandboxEndpoint:
            del sandbox_id, port, use_server_proxy
            await asyncio.Event().wait()
            raise AssertionError("unreachable")

        async def kill_sandbox(self, sandbox_id: str) -> None:
            self.killed.append(sandbox_id)

    class _FactoryStub:
        def __init__(self, connection_config: ConnectionConfig) -> None:
            self.connection_config = connection_config

        def create_sandbox_service(self):
            return sandbox_service

    sandbox_service = _SandboxServiceCreateStub()
    monkeypatch.setattr("opensandbox.sandbox.AdapterFactory", _FactoryStub)

    task = asyncio.create_task(
        Sandbox.create("python:3.11", connection_config=ConnectionConfig())
    )
    await asyncio.wait_for(sandbox_service.created.wait(), timeout=1)
    task.cancel()

    with pytest.raises(asyncio.CancelledError):
        await task

    assert sandbox_service.killed == ["sbx-created-before-cancel"]


@pytest.mark.asyncio
async def test_create_preserves_manual_cleanup_timeout(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class _CreateResponse:
        id = "sbx-created"

    class _SandboxServiceCreateStub:
        def __init__(self) -> None:
            self.create_calls: list[tuple[tuple[object, ...], dict[str, object]]] = []

        async def create_sandbox(self, *args, **kwargs):
            self.create_calls.append((args, kwargs))
            return _CreateResponse()

        async def get_sandbox_endpoint(
            self, _sandbox_id, port: int, _use_server_proxy: bool = False
        ) -> SandboxEndpoint:
            return SandboxEndpoint(endpoint=f"sbx.internal:{port}")

        async def kill_sandbox(self, _sandbox_id: str) -> None:
            return None

    class _FactoryStub:
        def __init__(self, _connection_config: ConnectionConfig) -> None:
            pass

        def create_sandbox_service(self):
            return sandbox_service

        def create_filesystem_service(self, _endpoint: SandboxEndpoint):
            return _Noop()

        def create_command_service(self, _endpoint: SandboxEndpoint):
            return _Noop()

        def create_health_service(self, _endpoint: SandboxEndpoint):
            return _Noop()

        def create_metrics_service(self, _endpoint: SandboxEndpoint):
            return _Noop()

        def create_egress_service(self, _endpoint: SandboxEndpoint):
            return _EgressServiceStub()

        def create_diagnostics_service(self):
            return _DiagnosticsServiceStub()

    sandbox_service = _SandboxServiceCreateStub()
    monkeypatch.setattr("opensandbox.sandbox.AdapterFactory", _FactoryStub)

    sandbox = await Sandbox.create(
        "python:3.11",
        timeout=None,
        skip_health_check=True,
        connection_config=ConnectionConfig(),
    )

    assert sandbox.id == "sbx-created"
    assert len(sandbox_service.create_calls) == 1
    args, kwargs = sandbox_service.create_calls[0]
    assert args == ()
    assert kwargs["timeout"] is None


@pytest.mark.asyncio
async def test_create_passes_new_signature_keywords_even_when_unused(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class _CreateResponse:
        id = "sbx-created"

    class _SandboxServiceCreateStub:
        async def create_sandbox(
            self,
            spec,
            entrypoint,
            env,
            metadata,
            timeout,
            resource,
            network_policy,
            extensions,
            volumes,
            platform=None,
            secure_access=False,
            snapshot_id=None,
        ):
            assert spec is not None
            assert entrypoint is not None
            assert isinstance(env, dict)
            assert isinstance(metadata, dict)
            assert timeout is not None
            assert isinstance(resource, dict)
            assert isinstance(network_policy, NetworkPolicy)
            assert isinstance(extensions, dict)
            assert volumes is None
            assert platform is None
            assert secure_access is False
            assert snapshot_id is None
            return _CreateResponse()

        async def get_sandbox_endpoint(self, _sandbox_id, port: int, _use_server_proxy: bool = False):
            return SandboxEndpoint(endpoint=f"sbx.internal:{port}")

        async def kill_sandbox(self, _sandbox_id: str) -> None:
            return None

    class _FactoryStub:
        def __init__(self, _connection_config: ConnectionConfig) -> None:
            pass

        def create_sandbox_service(self):
            return _SandboxServiceCreateStub()

        def create_filesystem_service(self, _endpoint):
            return _Noop()

        def create_command_service(self, _endpoint):
            return _Noop()

        def create_health_service(self, _endpoint):
            return _Noop()

        def create_metrics_service(self, _endpoint):
            return _Noop()

        def create_egress_service(self, _endpoint):
            return _EgressServiceStub()

        def create_diagnostics_service(self):
            return _DiagnosticsServiceStub()

    monkeypatch.setattr("opensandbox.sandbox.AdapterFactory", _FactoryStub)
    await Sandbox.create(
        "python:3.11",
        network_policy=NetworkPolicy(
            defaultAction="deny",
            egress=[NetworkRule(action="allow", target="pypi.org")],
        ),
        skip_health_check=True,
    )


@pytest.mark.asyncio
async def test_create_restore_from_snapshot_passes_snapshot_id(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class _CreateResponse:
        id = "sbx-created"

    class _SandboxServiceCreateStub:
        def __init__(self) -> None:
            self.create_calls: list[tuple[object, object]] = []

        async def create_sandbox(
            self,
            spec,
            entrypoint,
            env,
            metadata,
            timeout,
            resource,
            network_policy,
            extensions,
            volumes,
            platform=None,
            secure_access=False,
            snapshot_id=None,
        ):
            self.create_calls.append((spec, entrypoint))
            assert isinstance(env, dict)
            assert isinstance(metadata, dict)
            assert timeout is not None
            assert isinstance(resource, dict)
            assert network_policy is None
            assert isinstance(extensions, dict)
            assert volumes is None
            assert platform is None
            assert secure_access is False
            assert snapshot_id == "snap-123"
            assert spec is None
            assert entrypoint == ["tail", "-f", "/dev/null"]
            return _CreateResponse()

        async def get_sandbox_endpoint(self, _sandbox_id, port: int, _use_server_proxy: bool = False):
            return SandboxEndpoint(endpoint=f"sbx.internal:{port}")

        async def kill_sandbox(self, _sandbox_id: str) -> None:
            return None

    class _FactoryStub:
        def __init__(self, _connection_config: ConnectionConfig) -> None:
            self.service = _SandboxServiceCreateStub()

        def create_sandbox_service(self):
            return self.service

        def create_filesystem_service(self, _endpoint):
            return _Noop()

        def create_command_service(self, _endpoint):
            return _Noop()

        def create_health_service(self, _endpoint):
            return _Noop()

        def create_metrics_service(self, _endpoint):
            return _Noop()

        def create_egress_service(self, _endpoint):
            return _EgressServiceStub()

        def create_diagnostics_service(self):
            return _DiagnosticsServiceStub()

    monkeypatch.setattr("opensandbox.sandbox.AdapterFactory", _FactoryStub)
    await Sandbox.create(snapshot_id="snap-123", skip_health_check=True)


@pytest.mark.asyncio
async def test_create_restore_from_snapshot_preserves_custom_entrypoint(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class _CreateResponse:
        id = "sbx-created"

    class _SandboxServiceCreateStub:
        async def create_sandbox(
            self,
            spec,
            entrypoint,
            env,
            metadata,
            timeout,
            resource,
            network_policy,
            extensions,
            volumes,
            platform=None,
            secure_access=False,
            snapshot_id=None,
        ):
            assert isinstance(env, dict)
            assert isinstance(metadata, dict)
            assert timeout is not None
            assert isinstance(resource, dict)
            assert network_policy is None
            assert isinstance(extensions, dict)
            assert volumes is None
            assert platform is None
            assert secure_access is False
            assert snapshot_id == "snap-123"
            assert spec is None
            assert entrypoint == ["python", "app.py"]
            return _CreateResponse()

        async def get_sandbox_endpoint(self, _sandbox_id, port: int, _use_server_proxy: bool = False):
            return SandboxEndpoint(endpoint=f"sbx.internal:{port}")

        async def kill_sandbox(self, _sandbox_id: str) -> None:
            return None

    class _FactoryStub:
        def __init__(self, _connection_config: ConnectionConfig) -> None:
            pass

        def create_sandbox_service(self):
            return _SandboxServiceCreateStub()

        def create_filesystem_service(self, _endpoint):
            return _Noop()

        def create_command_service(self, _endpoint):
            return _Noop()

        def create_health_service(self, _endpoint):
            return _Noop()

        def create_metrics_service(self, _endpoint):
            return _Noop()

        def create_egress_service(self, _endpoint):
            return _EgressServiceStub()

        def create_diagnostics_service(self):
            return _DiagnosticsServiceStub()

    monkeypatch.setattr("opensandbox.sandbox.AdapterFactory", _FactoryStub)
    await Sandbox.create(
        snapshot_id="snap-123",
        entrypoint=["python", "app.py"],
        skip_health_check=True,
    )
