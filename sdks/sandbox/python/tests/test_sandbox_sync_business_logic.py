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

from datetime import timedelta
from uuid import uuid4

import pytest

from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.constants import DEFAULT_EGRESS_PORT, DEFAULT_EXECD_PORT
from opensandbox.exceptions import SandboxReadyTimeoutException
from opensandbox.models.diagnostics import DiagnosticContent
from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule, SandboxEndpoint
from opensandbox.sync.sandbox import SandboxSync


class _Noop:
    pass


class _SandboxServiceStub:
    def __init__(self) -> None:
        self.endpoint_calls: list[tuple[object, int, bool]] = []

    def get_sandbox_endpoint(self, sandbox_id, port: int, use_server_proxy: bool = False) -> SandboxEndpoint:
        self.endpoint_calls.append((sandbox_id, port, use_server_proxy))
        return SandboxEndpoint(endpoint=f"sync-egress:{port}", headers={"X-Egress": "1"})


class _EgressServiceStub:
    def __init__(self) -> None:
        self.patch_calls: list[list[NetworkRule]] = []

    def get_policy(self) -> NetworkPolicy:
        return NetworkPolicy(
            defaultAction="deny",
            egress=[NetworkRule(action="allow", target="pypi.org")],
        )

    def patch_rules(self, rules: list[NetworkRule]) -> None:
        self.patch_calls.append(rules)


class _DiagnosticsServiceStub:
    def __init__(self) -> None:
        self.calls: list[tuple[str, object, str | None]] = []

    def get_logs(self, sandbox_id: str, scope: str) -> DiagnosticContent:
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

    def get_events(self, sandbox_id: str, scope: str) -> DiagnosticContent:
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


def test_sync_check_ready_timeout_message_includes_troubleshooting_hints() -> None:
    def _always_false(_: SandboxSync) -> bool:
        return False

    sbx = SandboxSync(
        sandbox_id=str(uuid4()),
        sandbox_service=_Noop(),
        filesystem_service=_Noop(),
        command_service=_Noop(),
        health_service=_Noop(),
        metrics_service=_Noop(),
        egress_service=_EgressServiceStub(),
        diagnostics_service=_DiagnosticsServiceStub(),
        connection_config=ConnectionConfigSync(
            domain="10.0.0.2:8080",
            use_server_proxy=False,
        ),
        custom_health_check=_always_false,
    )

    with pytest.raises(SandboxReadyTimeoutException) as exc_info:
        sbx.check_ready(timeout=timedelta(seconds=0.01), polling_interval=timedelta(seconds=0))

    message = str(exc_info.value)
    assert "ConnectionConfig(domain=10.0.0.2:8080, use_server_proxy=False)" in message
    assert "ConnectionConfigSync(use_server_proxy=True)" in message


def test_sync_get_egress_policy_uses_injected_egress_service() -> None:
    sbx = SandboxSync(
        sandbox_id=str(uuid4()),
        sandbox_service=_SandboxServiceStub(),
        filesystem_service=_Noop(),
        command_service=_Noop(),
        health_service=_Noop(),
        metrics_service=_Noop(),
        egress_service=_EgressServiceStub(),
        diagnostics_service=_DiagnosticsServiceStub(),
        connection_config=ConnectionConfigSync(use_server_proxy=True),
    )

    policy = sbx.get_egress_policy()

    assert policy.default_action == "deny"
    assert policy.egress is not None
    assert policy.egress[0].target == "pypi.org"


def test_sync_patch_egress_rules_uses_injected_egress_service() -> None:
    svc = _SandboxServiceStub()
    egress_service = _EgressServiceStub()

    sbx = SandboxSync(
        sandbox_id=str(uuid4()),
        sandbox_service=svc,
        filesystem_service=_Noop(),
        command_service=_Noop(),
        health_service=_Noop(),
        metrics_service=_Noop(),
        egress_service=egress_service,
        diagnostics_service=_DiagnosticsServiceStub(),
        connection_config=ConnectionConfigSync(use_server_proxy=False),
    )
    rules = [NetworkRule(action="allow", target="www.github.com")]

    sbx.patch_egress_rules(rules)

    assert svc.endpoint_calls == []
    assert egress_service.patch_calls == [rules]


def test_sync_get_diagnostics_uses_injected_diagnostics_service() -> None:
    diagnostics_service = _DiagnosticsServiceStub()
    sbx = SandboxSync(
        sandbox_id=str(uuid4()),
        sandbox_service=_SandboxServiceStub(),
        filesystem_service=_Noop(),
        command_service=_Noop(),
        health_service=_Noop(),
        metrics_service=_Noop(),
        egress_service=_EgressServiceStub(),
        diagnostics_service=diagnostics_service,
        connection_config=ConnectionConfigSync(use_server_proxy=True),
    )

    logs = sbx.get_diagnostic_logs(scope="container")
    events = sbx.diagnostics.get_events(sbx.id, scope="runtime")

    assert logs.kind == "logs"
    assert events.kind == "events"
    assert diagnostics_service.calls == [
        ("logs", sbx.id, "container"),
        ("events", sbx.id, "runtime"),
    ]


def test_sync_create_resolves_egress_endpoint_and_builds_service(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    egress_service = _EgressServiceStub()
    factory_calls: list[SandboxEndpoint] = []

    class _CreateResponse:
        id = "sync-created"

    class _SandboxServiceCreateStub:
        def __init__(self) -> None:
            self.endpoint_calls: list[tuple[str, int, bool]] = []

        def create_sandbox(self, *_args, **_kwargs):
            return _CreateResponse()

        def get_sandbox_endpoint(self, sandbox_id, port: int, use_server_proxy: bool = False) -> SandboxEndpoint:
            self.endpoint_calls.append((sandbox_id, port, use_server_proxy))
            return SandboxEndpoint(endpoint=f"sync-egress:{port}", headers={"X-Port": str(port)})

        def kill_sandbox(self, _sandbox_id: str) -> None:
            return None

    class _FactoryStub:
        def __init__(self, connection_config: ConnectionConfigSync) -> None:
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
    monkeypatch.setattr("opensandbox.sync.sandbox.AdapterFactorySync", _FactoryStub)

    SandboxSync.create(
        "python:3.11",
        connection_config=ConnectionConfigSync(use_server_proxy=False),
        health_check=lambda _sbx: True,
    )

    assert sandbox_service.endpoint_calls == [
        ("sync-created", DEFAULT_EXECD_PORT, False),
        ("sync-created", DEFAULT_EGRESS_PORT, False),
    ]
    assert len(factory_calls) == 1
    assert factory_calls == [
        SandboxEndpoint(
            endpoint=f"sync-egress:{DEFAULT_EGRESS_PORT}",
            headers={"X-Port": str(DEFAULT_EGRESS_PORT)},
        )
    ]


def test_sync_create_passes_new_signature_keywords_even_when_unused(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class _CreateResponse:
        id = "sync-created"

    class _SandboxServiceCreateStub:
        def create_sandbox(
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

        def get_sandbox_endpoint(self, _sandbox_id, port: int, _use_server_proxy: bool = False):
            return SandboxEndpoint(endpoint=f"sync-egress:{port}")

        def kill_sandbox(self, _sandbox_id: str) -> None:
            return None

    class _FactoryStub:
        def __init__(self, _connection_config: ConnectionConfigSync) -> None:
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

    monkeypatch.setattr("opensandbox.sync.sandbox.AdapterFactorySync", _FactoryStub)
    SandboxSync.create(
        "python:3.11",
        network_policy=NetworkPolicy(
            defaultAction="deny",
            egress=[NetworkRule(action="allow", target="pypi.org")],
        ),
        skip_health_check=True,
    )


def test_sync_create_preserves_manual_cleanup_timeout(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class _CreateResponse:
        id = "sync-created"

    class _SandboxServiceCreateStub:
        def __init__(self) -> None:
            self.create_calls: list[tuple[tuple[object, ...], dict[str, object]]] = []

        def create_sandbox(self, *args, **kwargs):
            self.create_calls.append((args, kwargs))
            return _CreateResponse()

        def get_sandbox_endpoint(
            self, _sandbox_id, port: int, _use_server_proxy: bool = False
        ) -> SandboxEndpoint:
            return SandboxEndpoint(endpoint=f"sync-egress:{port}")

        def kill_sandbox(self, _sandbox_id: str) -> None:
            return None

    class _FactoryStub:
        def __init__(self, _connection_config: ConnectionConfigSync) -> None:
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
    monkeypatch.setattr("opensandbox.sync.sandbox.AdapterFactorySync", _FactoryStub)

    sandbox = SandboxSync.create(
        "python:3.11",
        timeout=None,
        skip_health_check=True,
        connection_config=ConnectionConfigSync(),
    )

    assert sandbox.id == "sync-created"
    assert len(sandbox_service.create_calls) == 1
    args, kwargs = sandbox_service.create_calls[0]
    assert args == ()
    assert kwargs["timeout"] is None


def test_sync_create_restore_from_snapshot_passes_snapshot_id(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class _CreateResponse:
        id = "sync-created"

    class _SandboxServiceCreateStub:
        def create_sandbox(
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
            assert entrypoint == ["tail", "-f", "/dev/null"]
            return _CreateResponse()

        def get_sandbox_endpoint(self, _sandbox_id, port: int, _use_server_proxy: bool = False):
            return SandboxEndpoint(endpoint=f"sync-egress:{port}")

        def kill_sandbox(self, _sandbox_id: str) -> None:
            return None

    class _FactoryStub:
        def __init__(self, _connection_config: ConnectionConfigSync) -> None:
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

    monkeypatch.setattr("opensandbox.sync.sandbox.AdapterFactorySync", _FactoryStub)
    SandboxSync.create(snapshot_id="snap-123", skip_health_check=True)


def test_sync_create_restore_from_snapshot_preserves_custom_entrypoint(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    class _CreateResponse:
        id = "sync-created"

    class _SandboxServiceCreateStub:
        def create_sandbox(
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

        def get_sandbox_endpoint(self, _sandbox_id, port: int, _use_server_proxy: bool = False):
            return SandboxEndpoint(endpoint=f"sync-egress:{port}")

        def kill_sandbox(self, _sandbox_id: str) -> None:
            return None

    class _FactoryStub:
        def __init__(self, _connection_config: ConnectionConfigSync) -> None:
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

    monkeypatch.setattr("opensandbox.sync.sandbox.AdapterFactorySync", _FactoryStub)
    SandboxSync.create(
        snapshot_id="snap-123",
        entrypoint=["python", "app.py"],
        skip_health_check=True,
    )
