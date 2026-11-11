#
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
#
from __future__ import annotations

from datetime import datetime, timezone

import pytest

from opensandbox.adapters.sandboxes_adapter import SandboxesAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.config.connection_sync import ConnectionConfigSync
from opensandbox.exceptions import SandboxApiException
from opensandbox.manager import SandboxManager
from opensandbox.models.templates import (
    CreateTemplateRequest,
    TemplateFilter,
    TemplatePhase,
    TemplateReadiness,
)
from opensandbox.sync.adapters.sandboxes_adapter import (
    SandboxesAdapterSync as SyncSandboxesAdapter,
)
from opensandbox.sync.manager import SandboxManagerSync


class _Resp:
    def __init__(self, *, status_code: int, parsed) -> None:
        self.status_code = status_code
        self.parsed = parsed


def _api_template(template_id: str = "tpl_1", phase: str = "Succeeded"):
    from opensandbox.api.lifecycle.models.fsb_template import FsbTemplate
    from opensandbox.api.lifecycle.models.fsb_template_env import FsbTemplateEnv
    from opensandbox.api.lifecycle.models.fsb_template_format import FsbTemplateFormat
    from opensandbox.api.lifecycle.models.fsb_template_metadata import (
        FsbTemplateMetadata,
    )
    from opensandbox.api.lifecycle.models.fsb_template_readiness import (
        FsbTemplateReadiness,
    )
    from opensandbox.api.lifecycle.models.fsb_template_status import FsbTemplateStatus
    from opensandbox.api.lifecycle.models.fsb_template_status_phase import (
        FsbTemplateStatusPhase,
    )
    from opensandbox.api.lifecycle.models.resource_limits import ResourceLimits

    return FsbTemplate(
        template_id=template_id,
        image="ubuntu:22.04",
        publish="s3://bucket/publish",
        format_=FsbTemplateFormat.OVERLAYBD,
        status=FsbTemplateStatus(
            phase=FsbTemplateStatusPhase(phase),
            manifest_ref="s3://bucket/publish/manifest.json"
            if phase == "Succeeded"
            else None,
            message="boom" if phase == "Failed" else None,
        ),
        created_at=datetime(2025, 1, 1, tzinfo=timezone.utc),
        updated_at=datetime(2025, 1, 2, tzinfo=timezone.utc),
        resource_limits=ResourceLimits.from_dict({"cpu": "2", "disk": "10Gi"}),
        entrypoint=["/bin/sh", "-c", "sleep 1"],
        env=FsbTemplateEnv.from_dict({"LOG_LEVEL": "info"}),
        metadata=FsbTemplateMetadata.from_dict({"env": "prod"}),
        readiness=FsbTemplateReadiness(probe="tcp://127.0.0.1:44772", warmup_seconds=30),
    )


def _api_list_templates_response():
    from opensandbox.api.lifecycle.models.list_fsb_templates_response import (
        ListFsbTemplatesResponse,
    )
    from opensandbox.api.lifecycle.models.pagination_info import PaginationInfo

    return ListFsbTemplatesResponse(
        items=[_api_template("tpl_1"), _api_template("tpl_2", phase="Pending")],
        pagination=PaginationInfo(
            page=1,
            page_size=20,
            total_items=2,
            total_pages=1,
            has_next_page=False,
        ),
    )


@pytest.mark.asyncio
async def test_create_template_maps_request_and_response(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured = {}

    async def _fake_asyncio_detailed(*, client, body):
        captured["body"] = body.to_dict()
        return _Resp(status_code=201, parsed=_api_template("tpl_new", phase="Pending"))

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.templates.create_template.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig(domain="example.com:8080", api_key="k"))
    created = await adapter.create_template(
        CreateTemplateRequest(
            image="ubuntu:22.04",
            publish="s3://bucket/publish",
            resourceLimits={"cpu": "2", "disk": "10Gi"},
            entrypoint=["/bin/sh", "-c", "sleep 1"],
            env={"LOG_LEVEL": "info"},
            metadata={"env": "prod"},
            readiness=TemplateReadiness(probe="tcp://127.0.0.1:44772", warmupSeconds=30),
        )
    )

    assert captured["body"] == {
        "image": "ubuntu:22.04",
        "publish": "s3://bucket/publish",
        "resourceLimits": {"cpu": "2", "disk": "10Gi"},
        "entrypoint": ["/bin/sh", "-c", "sleep 1"],
        "env": {"LOG_LEVEL": "info"},
        "metadata": {"env": "prod"},
        "readiness": {"probe": "tcp://127.0.0.1:44772", "warmupSeconds": 30},
    }
    assert created.template_id == "tpl_new"
    assert created.status.phase == TemplatePhase.PENDING
    assert created.status.manifest_ref is None
    assert created.resource_limits == {"cpu": "2", "disk": "10Gi"}
    assert created.env == {"LOG_LEVEL": "info"}
    assert created.readiness is not None
    assert created.readiness.warmup_seconds == 30


@pytest.mark.asyncio
async def test_get_template_converts_succeeded_status(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured = {}

    async def _fake_asyncio_detailed(*, client, template_id):
        captured["template_id"] = template_id
        return _Resp(status_code=200, parsed=_api_template("tpl_1"))

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.templates.get_template.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    loaded = await adapter.get_template("tpl_1")

    assert captured["template_id"] == "tpl_1"
    assert loaded.template_id == "tpl_1"
    assert loaded.format == "overlaybd"
    assert loaded.status.phase == TemplatePhase.SUCCEEDED
    assert loaded.status.manifest_ref == "s3://bucket/publish/manifest.json"
    assert loaded.env == {"LOG_LEVEL": "info"}
    assert loaded.metadata == {"env": "prod"}
    assert loaded.created_at == datetime(2025, 1, 1, tzinfo=timezone.utc)


@pytest.mark.asyncio
async def test_list_templates_joins_metadata_and_converts_pagination(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured = {}

    async def _fake_asyncio_detailed(*, client, metadata, page, page_size):
        captured.update(
            {"metadata": metadata, "page": page, "page_size": page_size}
        )
        return _Resp(status_code=200, parsed=_api_list_templates_response())

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.templates.list_templates.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    listed = await adapter.list_templates(
        TemplateFilter(metadata={"env": "prod"}, page=1, pageSize=20)
    )

    assert captured["metadata"] == "env=prod"
    assert captured["page"] == 1
    assert captured["page_size"] == 20
    assert [t.template_id for t in listed.template_infos] == ["tpl_1", "tpl_2"]
    assert listed.template_infos[1].status.phase == TemplatePhase.PENDING
    assert listed.pagination.total_items == 2


@pytest.mark.asyncio
async def test_list_templates_metadata_percent_encoded(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from urllib.parse import parse_qsl

    captured = {}

    async def _fake_asyncio_detailed(*, client, metadata, page, page_size):
        captured["metadata"] = metadata
        return _Resp(status_code=200, parsed=_api_list_templates_response())

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.templates.list_templates.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    raw = {"a": "x&y=b", "p": "50%"}
    await adapter.list_templates(TemplateFilter(metadata=raw))

    assert dict(parse_qsl(captured["metadata"])) == raw


def test_sync_list_templates_metadata_percent_encoded(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from urllib.parse import parse_qsl

    captured = {}

    def _fake_sync_detailed(*, client, metadata, page, page_size):
        captured["metadata"] = metadata
        return _Resp(status_code=200, parsed=_api_list_templates_response())

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.templates.list_templates.sync_detailed",
        _fake_sync_detailed,
    )

    adapter = SyncSandboxesAdapter(ConnectionConfigSync())
    raw = {"a": "x&y=b"}
    adapter.list_templates(TemplateFilter(metadata=raw))

    assert dict(parse_qsl(captured["metadata"])) == raw


def test_template_filter_rejects_zero_page() -> None:
    import pytest as _pytest
    from pydantic import ValidationError

    with _pytest.raises(ValidationError, match="at least 1"):
        TemplateFilter(page=0)


@pytest.mark.asyncio
async def test_list_templates_omits_unset_filters(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from opensandbox.api.lifecycle.types import UNSET as API_UNSET

    captured = {}

    async def _fake_asyncio_detailed(*, client, metadata, page, page_size):
        captured.update(
            {"metadata": metadata, "page": page, "page_size": page_size}
        )
        return _Resp(status_code=200, parsed=_api_list_templates_response())

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.templates.list_templates.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    await adapter.list_templates(TemplateFilter())

    assert captured["metadata"] is API_UNSET
    assert captured["page"] is API_UNSET
    assert captured["page_size"] is API_UNSET


@pytest.mark.asyncio
async def test_delete_template_calls_openapi(monkeypatch: pytest.MonkeyPatch) -> None:
    captured = {}

    async def _fake_asyncio_detailed(*, client, template_id):
        captured["template_id"] = template_id
        return _Resp(status_code=204, parsed=None)

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.templates.delete_template.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    await adapter.delete_template("tpl_1")

    assert captured["template_id"] == "tpl_1"


@pytest.mark.asyncio
async def test_create_template_conflict_maps_to_sandbox_exception(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from opensandbox.api.lifecycle.models.error_response import ErrorResponse

    async def _fake_asyncio_detailed(*, client, body):
        return _Resp(
            status_code=409,
            parsed=ErrorResponse(
                code="TEMPLATE_ALREADY_EXISTS",
                message="A template build with the same name already exists",
            ),
        )

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.templates.create_template.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.create_template(
            CreateTemplateRequest(
                image="ubuntu:22.04", publish="s3://bucket/publish"
            )
        )

    assert exc_info.value.status_code == 409
    assert exc_info.value.error.code == "TEMPLATE_ALREADY_EXISTS"


def test_sync_template_lifecycle_calls_openapi(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from opensandbox.api.lifecycle.types import UNSET as API_UNSET

    calls: list[tuple[str, object]] = []

    def _create(*, client, body):
        calls.append(("create", body.to_dict()))
        return _Resp(status_code=201, parsed=_api_template("tpl_new", phase="Pending"))

    def _get(*, client, template_id):
        calls.append(("get", template_id))
        return _Resp(status_code=200, parsed=_api_template(template_id))

    def _list(*, client, metadata, page, page_size):
        calls.append(("list", (metadata, page, page_size)))
        return _Resp(status_code=200, parsed=_api_list_templates_response())

    def _delete(*, client, template_id):
        calls.append(("delete", template_id))
        return _Resp(status_code=204, parsed=None)

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.templates.create_template.sync_detailed",
        _create,
    )
    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.templates.get_template.sync_detailed",
        _get,
    )
    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.templates.list_templates.sync_detailed",
        _list,
    )
    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.templates.delete_template.sync_detailed",
        _delete,
    )

    adapter = SyncSandboxesAdapter(ConnectionConfigSync())
    created = adapter.create_template(
        CreateTemplateRequest(image="ubuntu:22.04", publish="s3://bucket/publish")
    )
    loaded = adapter.get_template("tpl_1")
    listed = adapter.list_templates(TemplateFilter(metadata={"env": "prod"}))
    adapter.delete_template("tpl_1")

    assert created.template_id == "tpl_new"
    assert loaded.template_id == "tpl_1"
    assert listed.pagination.total_items == 2
    assert calls == [
        (
            "create",
            {"image": "ubuntu:22.04", "publish": "s3://bucket/publish"},
        ),
        ("get", "tpl_1"),
        ("list", ("env=prod", API_UNSET, API_UNSET)),
        ("delete", "tpl_1"),
    ]


class _TemplatesServiceStub:
    def __init__(self) -> None:
        self.template_calls: list[tuple[str, object]] = []

    async def create_template(self, request):
        self.template_calls.append(("create", request))
        return type("Template", (), {"template_id": "tpl_new"})()

    async def get_template(self, template_id):
        self.template_calls.append(("get", template_id))
        return type("Template", (), {"template_id": template_id})()

    async def list_templates(self, filter):
        self.template_calls.append(("list", filter))
        return type("Paged", (), {"template_infos": []})()

    async def delete_template(self, template_id):
        self.template_calls.append(("delete", template_id))


@pytest.mark.asyncio
async def test_manager_delegates_template_operations() -> None:
    stub = _TemplatesServiceStub()
    manager = SandboxManager(stub, ConnectionConfig())

    request = CreateTemplateRequest(
        image="ubuntu:22.04", publish="s3://bucket/publish"
    )
    list_filter = TemplateFilter()
    created = await manager.create_template(request)
    loaded = await manager.get_template("tpl_1")
    listed = await manager.list_templates(list_filter)
    await manager.delete_template("tpl_1")

    assert created.template_id == "tpl_new"
    assert loaded.template_id == "tpl_1"
    assert listed.template_infos == []
    assert stub.template_calls == [
        ("create", request),
        ("get", "tpl_1"),
        ("list", list_filter),
        ("delete", "tpl_1"),
    ]


def test_sync_manager_delegates_template_operations() -> None:
    class _SyncStub(_TemplatesServiceStub):
        def create_template(self, request):  # type: ignore[override]
            self.template_calls.append(("create", request))
            return type("Template", (), {"template_id": "tpl_new"})()

        def get_template(self, template_id):  # type: ignore[override]
            self.template_calls.append(("get", template_id))
            return type("Template", (), {"template_id": template_id})()

        def list_templates(self, filter):  # type: ignore[override]
            self.template_calls.append(("list", filter))
            return type("Paged", (), {"template_infos": []})()

        def delete_template(self, template_id):  # type: ignore[override]
            self.template_calls.append(("delete", template_id))

    stub = _SyncStub()
    manager = SandboxManagerSync(stub, ConnectionConfigSync(), None)

    created = manager.create_template(
        CreateTemplateRequest(image="ubuntu:22.04", publish="s3://bucket/publish")
    )
    loaded = manager.get_template("tpl_1")
    manager.list_templates(TemplateFilter())
    manager.delete_template("tpl_1")

    assert created.template_id == "tpl_new"
    assert loaded.template_id == "tpl_1"
    assert [c[0] for c in stub.template_calls] == [
        "create",
        "get",
        "list",
        "delete",
    ]


def _api_create_sandbox_response(sandbox_id: str):
    from opensandbox.api.lifecycle.models.create_sandbox_response import (
        CreateSandboxResponse,
    )
    from opensandbox.api.lifecycle.models.sandbox_status import SandboxStatus

    return CreateSandboxResponse(
        id=sandbox_id,
        status=SandboxStatus(state="Running"),
        expires_at=datetime(2025, 1, 2, tzinfo=timezone.utc),
        created_at=datetime(2025, 1, 1, tzinfo=timezone.utc),
        entrypoint=["/bin/sh"],
    )


@pytest.mark.asyncio
async def test_create_sandbox_from_template_maps_wire_body(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from datetime import timedelta

    from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule

    captured = {}

    async def _fake_asyncio_detailed(*, client, body):
        captured["body"] = body.to_dict()
        return _Resp(
            status_code=201, parsed=_api_create_sandbox_response("sbx-from-template")
        )

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.post_sandboxes.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    response = await adapter.create_sandbox_from_template(
        "tpl_1",
        timeout=timedelta(minutes=5),
        metadata={"team": "platform"},
        network_policy=NetworkPolicy(
            defaultAction="deny",
            egress=[NetworkRule(action="allow", target="pypi.org")],
        ),
        extensions={"debug": "true"},
    )

    assert response.id == "sbx-from-template"
    body = captured["body"]
    assert body["templateId"] == "tpl_1"
    assert body["timeout"] == 300
    assert body["metadata"] == {"team": "platform"}
    assert body["extensions"] == {"debug": "true"}
    assert body["networkPolicy"] == {
        "defaultAction": "deny",
        "egress": [{"action": "allow", "target": "pypi.org"}],
    }
    # Template mode: workload-shaping fields must be absent from the wire body.
    # (secureAccess is always serialized by the generated model; false is valid.)
    assert body["secureAccess"] is False
    for absent in (
        "image",
        "snapshotId",
        "entrypoint",
        "env",
        "resourceLimits",
        "resourceRequests",
        "volumes",
        "platform",
        "credentialProxy",
        "lifecycle",
    ):
        assert absent not in body, f"{absent} must not be sent in template mode"


def test_sync_create_sandbox_from_template_maps_wire_body(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from datetime import timedelta

    captured = {}

    def _fake_sync_detailed(*, client, body):
        captured["body"] = body.to_dict()
        return _Resp(
            status_code=201, parsed=_api_create_sandbox_response("sbx-from-template")
        )

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.post_sandboxes.sync_detailed",
        _fake_sync_detailed,
    )

    adapter = SyncSandboxesAdapter(ConnectionConfigSync())
    response = adapter.create_sandbox_from_template(
        "tpl_1",
        timeout=timedelta(minutes=5),
    )

    assert response.id == "sbx-from-template"
    body = captured["body"]
    assert body["templateId"] == "tpl_1"
    assert body["timeout"] == 300
    assert "resourceLimits" not in body
    assert "entrypoint" not in body


@pytest.mark.asyncio
async def test_endpoint_response_header_populates_origin(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from opensandbox.api.lifecycle.models.endpoint import Endpoint
    from opensandbox.api.lifecycle.models.endpoint_headers import EndpointHeaders

    class _RespWithHeaders:
        def __init__(self, *, status_code: int, parsed, headers) -> None:
            self.status_code = status_code
            self.parsed = parsed
            self.headers = headers

    async def _fake_asyncio_detailed(*, client, sandbox_id, port, use_server_proxy, expires=None):
        return _RespWithHeaders(
            status_code=200,
            parsed=Endpoint(
                endpoint=f"sbx.internal:{port}",
                headers=EndpointHeaders.from_dict({}),
            ),
            headers={"OPEN-SANDBOX-ORIGIN": "template"},
        )

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.get_sandboxes_sandbox_id_endpoints_port.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    endpoint = await adapter.get_sandbox_endpoint("sbx-tpl", 8080)

    assert endpoint.origin == "template"


def test_sync_endpoint_response_header_populates_origin(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from opensandbox.api.lifecycle.models.endpoint import Endpoint
    from opensandbox.api.lifecycle.models.endpoint_headers import EndpointHeaders

    class _RespWithHeaders:
        def __init__(self, *, status_code: int, parsed, headers) -> None:
            self.status_code = status_code
            self.parsed = parsed
            self.headers = headers

    def _fake_sync_detailed(*, sandbox_id, port, client, use_server_proxy, expires=None):
        return _RespWithHeaders(
            status_code=200,
            parsed=Endpoint(
                endpoint=f"sbx.internal:{port}",
                headers=EndpointHeaders.from_dict({}),
            ),
            headers={},
        )

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.get_sandboxes_sandbox_id_endpoints_port.sync_detailed",
        _fake_sync_detailed,
    )

    adapter = SyncSandboxesAdapter(ConnectionConfigSync())
    endpoint = adapter.get_sandbox_endpoint("sbx-plain", 8080)

    assert endpoint.origin is None
