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

from datetime import datetime, timedelta, timezone
from uuid import uuid4

import pytest

from opensandbox.adapters.sandboxes_adapter import SandboxesAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.sandboxes import (
    CreateSnapshotRequest,
    NetworkPolicy,
    NetworkRule,
    SandboxFilter,
    SandboxImageSpec,
    SnapshotFilter,
)


class _Resp:
    def __init__(self, *, status_code: int, parsed) -> None:
        self.status_code = status_code
        self.parsed = parsed


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


def _api_list_sandboxes_response():
    from opensandbox.api.lifecycle.models.image_spec import ImageSpec
    from opensandbox.api.lifecycle.models.list_sandboxes_response import (
        ListSandboxesResponse,
    )
    from opensandbox.api.lifecycle.models.pagination_info import PaginationInfo
    from opensandbox.api.lifecycle.models.sandbox import Sandbox
    from opensandbox.api.lifecycle.models.sandbox_status import SandboxStatus

    sbx = Sandbox(
        id=str(uuid4()),
        image=ImageSpec(uri="python:3.11"),
        status=SandboxStatus(state="Running"),
        entrypoint=["/bin/sh"],
        expires_at=datetime(2025, 1, 2, tzinfo=timezone.utc),
        created_at=datetime(2025, 1, 1, tzinfo=timezone.utc),
    )
    return ListSandboxesResponse(
        items=[sbx],
        pagination=PaginationInfo(
            page=0,
            page_size=10,
            total_items=1,
            total_pages=1,
            has_next_page=False,
        ),
    )


def _api_snapshot(snapshot_id: str):
    from opensandbox.api.lifecycle.models.snapshot import Snapshot
    from opensandbox.api.lifecycle.models.snapshot_status import SnapshotStatus

    return Snapshot(
        id=snapshot_id,
        sandbox_id="sbx-1",
        name="before-upgrade",
        status=SnapshotStatus(state="Ready"),
        created_at=datetime(2025, 1, 1, tzinfo=timezone.utc),
    )


def _api_sandbox(sandbox_id: str):
    from opensandbox.api.lifecycle.models.sandbox import Sandbox
    from opensandbox.api.lifecycle.models.sandbox_metadata import SandboxMetadata
    from opensandbox.api.lifecycle.models.sandbox_status import SandboxStatus

    metadata = SandboxMetadata.from_dict({"team": "platform"})
    return Sandbox(
        id=sandbox_id,
        status=SandboxStatus(state="Running"),
        metadata=metadata,
        entrypoint=["/bin/sh"],
        expires_at=datetime(2025, 1, 2, tzinfo=timezone.utc),
        created_at=datetime(2025, 1, 1, tzinfo=timezone.utc),
    )


@pytest.mark.asyncio
async def test_create_sandbox_success(monkeypatch: pytest.MonkeyPatch) -> None:
    called = {}

    async def _fake_asyncio_detailed(*, client, body):
        called["body"] = body
        return _Resp(status_code=200, parsed=_api_create_sandbox_response(str(uuid4())))

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.post_sandboxes.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    cfg = ConnectionConfig(domain="example.com:8080", api_key="k")
    adapter = SandboxesAdapter(cfg)

    out = await adapter.create_sandbox(
        spec=SandboxImageSpec("python:3.11"),
        entrypoint=["/bin/sh"],
        env={},
        metadata={},
        timeout=timedelta(seconds=3),
        resource={"cpu": "100m"},
        platform=None,
        network_policy=NetworkPolicy(
            defaultAction="deny",
            egress=[NetworkRule(action="allow", target="pypi.org")],
        ),
        extensions={"storage.id": "abc123", "debug": "true"},
        volumes=None,
        secure_access=True,
    )
    assert isinstance(out.id, str)
    assert "image" in called["body"].to_dict()
    assert called["body"].to_dict()["secureAccess"] is True
    assert called["body"].to_dict()["extensions"] == {"storage.id": "abc123", "debug": "true"}
    network_policy = called["body"].to_dict()["networkPolicy"]
    assert network_policy["defaultAction"] == "deny"
    assert network_policy["egress"] == [{"action": "allow", "target": "pypi.org"}]


@pytest.mark.asyncio
async def test_create_sandbox_manual_cleanup_preserves_null_timeout(monkeypatch: pytest.MonkeyPatch) -> None:
    called = {}

    async def _fake_asyncio_detailed(*, client, body):
        called["body"] = body
        return _Resp(status_code=200, parsed=_api_create_sandbox_response(str(uuid4())))

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.post_sandboxes.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig(domain="example.com:8080", api_key="k"))
    await adapter.create_sandbox(
        spec=SandboxImageSpec("python:3.11"),
        entrypoint=["/bin/sh"],
        env={},
        metadata={},
        timeout=None,
        resource={"cpu": "100m"},
        platform=None,
        network_policy=None,
        extensions={},
        volumes=None,
    )

    assert called["body"].to_dict()["timeout"] is None


@pytest.mark.asyncio
async def test_create_sandbox_restore_from_snapshot(monkeypatch: pytest.MonkeyPatch) -> None:
    called = {}

    async def _fake_asyncio_detailed(*, client, body):
        called["body"] = body
        return _Resp(status_code=200, parsed=_api_create_sandbox_response(str(uuid4())))

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.post_sandboxes.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig(domain="example.com:8080", api_key="k"))
    await adapter.create_sandbox(
        spec=None,
        entrypoint=None,
        env={},
        metadata={},
        timeout=None,
        resource={"cpu": "100m"},
        platform=None,
        network_policy=None,
        extensions={},
        volumes=None,
        snapshot_id="snap-123",
    )

    dumped = called["body"].to_dict()
    assert dumped["snapshotId"] == "snap-123"
    assert "image" not in dumped
    assert "entrypoint" not in dumped


@pytest.mark.asyncio
async def test_create_sandbox_http_error_preserves_wrapped_error_detail(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from opensandbox.api.lifecycle.models.error_response import ErrorResponse

    async def _fake_asyncio_detailed(*, client, body):
        return _Resp(
            status_code=409,
            parsed=ErrorResponse(
                code="K8S_API_ERROR",
                message="Failed to create sandbox: pool exhausted",
            ),
        )

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.post_sandboxes.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig(domain="example.com:8080", api_key="k"))

    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.create_sandbox(
            spec=SandboxImageSpec("python:3.11"),
            entrypoint=["/bin/sh"],
            env={},
            metadata={},
            timeout=timedelta(seconds=3),
            resource={"cpu": "100m"},
            platform=None,
            network_policy=None,
            extensions={},
            volumes=None,
        )

    exc = exc_info.value
    assert exc.status_code == 409
    assert exc.error.code == "K8S_API_ERROR"
    assert exc.error.message == "Failed to create sandbox: pool exhausted"
    assert "pool exhausted" in str(exc)


@pytest.mark.asyncio
async def test_create_sandbox_unexpected_status_preserves_fastapi_detail_payload(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    from opensandbox.api.lifecycle.errors import UnexpectedStatus

    async def _fake_asyncio_detailed(*, client, body):
        raise UnexpectedStatus(
            409,
            b'{"detail":{"code":"K8S_API_ERROR","message":"Failed to create sandbox: pool exhausted"}}',
        )

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.post_sandboxes.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig(domain="example.com:8080", api_key="k"))

    with pytest.raises(SandboxApiException) as exc_info:
        await adapter.create_sandbox(
            spec=SandboxImageSpec("python:3.11"),
            entrypoint=["/bin/sh"],
            env={},
            metadata={},
            timeout=timedelta(seconds=3),
            resource={"cpu": "100m"},
            platform=None,
            network_policy=None,
            extensions={},
            volumes=None,
        )

    exc = exc_info.value
    assert exc.status_code == 409
    assert exc.error is not None
    assert exc.error.code == "K8S_API_ERROR"
    assert exc.error.message == "Failed to create sandbox: pool exhausted"
    assert "HTTP 409" in str(exc)


@pytest.mark.asyncio
async def test_create_sandbox_empty_response_raises(monkeypatch: pytest.MonkeyPatch) -> None:
    async def _fake_asyncio_detailed(*, client, body):
        return _Resp(status_code=200, parsed=None)

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.post_sandboxes.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    with pytest.raises(SandboxApiException):
        await adapter.create_sandbox(
            spec=SandboxImageSpec("python:3.11"),
            entrypoint=["/bin/sh"],
            env={},
            metadata={},
            timeout=timedelta(seconds=1),
            resource={"cpu": "100m"},
            platform=None,
            extensions={"debug": "true"},
            network_policy=NetworkPolicy(),
            volumes=None,
        )


@pytest.mark.asyncio
async def test_list_sandboxes_metadata_double_encoded(monkeypatch: pytest.MonkeyPatch) -> None:
    from opensandbox.api.lifecycle.types import UNSET as API_UNSET

    captured = {}

    async def _fake_asyncio_detailed(*, client, state, metadata, page, page_size):
        captured.update(
            {"state": state, "metadata": metadata, "page": page, "page_size": page_size}
        )
        return _Resp(status_code=200, parsed=_api_list_sandboxes_response())

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.get_sandboxes.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    f = SandboxFilter(metadata={"k k": "v/v"})
    await adapter.list_sandboxes(f)

    assert captured["metadata"] == "k k=v/v"
    assert captured["state"] is API_UNSET


@pytest.mark.asyncio
async def test_pause_resume_kill_call_openapi(monkeypatch: pytest.MonkeyPatch) -> None:
    sbx_id = str(uuid4())
    calls: list[tuple[str, str]] = []

    async def _ok_pause(*, client, sandbox_id):
        calls.append(("pause", sandbox_id))
        return _Resp(status_code=204, parsed=None)

    async def _ok_resume(*, client, sandbox_id):
        calls.append(("resume", sandbox_id))
        return _Resp(status_code=204, parsed=None)

    async def _ok_kill(*, client, sandbox_id):
        calls.append(("kill", sandbox_id))
        return _Resp(status_code=204, parsed=None)

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.post_sandboxes_sandbox_id_pause.asyncio_detailed",
        _ok_pause,
    )
    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.post_sandboxes_sandbox_id_resume.asyncio_detailed",
        _ok_resume,
    )
    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.delete_sandboxes_sandbox_id.asyncio_detailed",
        _ok_kill,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    await adapter.pause_sandbox(sbx_id)
    await adapter.resume_sandbox(sbx_id)
    await adapter.kill_sandbox(sbx_id)

    assert calls == [("pause", sbx_id), ("resume", sbx_id), ("kill", sbx_id)]


@pytest.mark.asyncio
async def test_patch_sandbox_metadata_sends_metadata_body(monkeypatch: pytest.MonkeyPatch) -> None:
    sbx_id = str(uuid4())
    captured = {}

    async def _patch_metadata(*, client, sandbox_id, body):
        captured["sandbox_id"] = sandbox_id
        captured["body"] = body.to_dict()
        return _Resp(status_code=200, parsed=_api_sandbox(sbx_id))

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.patch_sandboxes_sandbox_id_metadata.asyncio_detailed",
        _patch_metadata,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    result = await adapter.patch_sandbox_metadata(
        sbx_id,
        {"team": "platform", "old": None},
    )

    assert captured == {
        "sandbox_id": sbx_id,
        "body": {"team": "platform", "old": None},
    }
    assert result.metadata == {"team": "platform"}


@pytest.mark.asyncio
async def test_renew_sandbox_expiration_sends_timezone_aware(monkeypatch: pytest.MonkeyPatch) -> None:
    captured = {}

    async def _fake_asyncio_detailed(*, client, sandbox_id, body):
        from opensandbox.api.lifecycle.models.renew_sandbox_expiration_response import (
            RenewSandboxExpirationResponse,
        )

        captured["expires_at"] = body.expires_at
        return _Resp(
            status_code=200,
            parsed=RenewSandboxExpirationResponse(expires_at=body.expires_at),
        )

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.sandboxes.post_sandboxes_sandbox_id_renew_expiration.asyncio_detailed",
        _fake_asyncio_detailed,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    await adapter.renew_sandbox_expiration(str(uuid4()), datetime(2025, 1, 1))  # naive

    assert captured["expires_at"].tzinfo is timezone.utc


@pytest.mark.asyncio
async def test_snapshot_lifecycle_calls_openapi(monkeypatch: pytest.MonkeyPatch) -> None:
    calls: list[tuple[str, object]] = []

    async def _create_snapshot(*, client, sandbox_id, body):
        calls.append(("create", (sandbox_id, body.name)))
        return _Resp(status_code=202, parsed=_api_snapshot("snap-1"))

    async def _get_snapshot(*, client, snapshot_id):
        calls.append(("get", snapshot_id))
        return _Resp(status_code=200, parsed=_api_snapshot(snapshot_id))

    async def _list_snapshots(*, client, sandbox_id, state, page, page_size):
        calls.append(("list", (sandbox_id, state, page, page_size)))
        from opensandbox.api.lifecycle.models.list_snapshots_response import (
            ListSnapshotsResponse,
        )
        from opensandbox.api.lifecycle.models.pagination_info import PaginationInfo

        return _Resp(
            status_code=200,
            parsed=ListSnapshotsResponse(
                items=[_api_snapshot("snap-1")],
                pagination=PaginationInfo(
                    page=1,
                    page_size=10,
                    total_items=1,
                    total_pages=1,
                    has_next_page=False,
                ),
            ),
        )

    async def _delete_snapshot(*, client, snapshot_id):
        calls.append(("delete", snapshot_id))
        return _Resp(status_code=204, parsed=None)

    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.snapshots.post_sandboxes_sandbox_id_snapshots.asyncio_detailed",
        _create_snapshot,
    )
    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.snapshots.get_snapshots_snapshot_id.asyncio_detailed",
        _get_snapshot,
    )
    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.snapshots.get_snapshots.asyncio_detailed",
        _list_snapshots,
    )
    monkeypatch.setattr(
        "opensandbox.api.lifecycle.api.snapshots.delete_snapshots_snapshot_id.asyncio_detailed",
        _delete_snapshot,
    )

    adapter = SandboxesAdapter(ConnectionConfig())
    created = await adapter.create_snapshot("sbx-1", CreateSnapshotRequest(name="before-upgrade"))
    loaded = await adapter.get_snapshot("snap-1")
    listed = await adapter.list_snapshots(
        SnapshotFilter(sandbox_id="sbx-1", states=["Ready"], page=1, page_size=10)
    )
    await adapter.delete_snapshot("snap-1")

    assert created.id == "snap-1"
    assert loaded.id == "snap-1"
    assert listed.snapshot_infos[0].id == "snap-1"
    assert calls == [
        ("create", ("sbx-1", "before-upgrade")),
        ("get", "snap-1"),
        ("list", ("sbx-1", ["Ready"], 1, 10)),
        ("delete", "snap-1"),
    ]
