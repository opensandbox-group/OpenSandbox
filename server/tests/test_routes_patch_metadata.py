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

from datetime import datetime, timedelta, timezone
from typing import Dict, Optional

from fastapi.exceptions import HTTPException
from fastapi.testclient import TestClient

from opensandbox_server.api import lifecycle
from opensandbox_server.api.schema import ImageSpec, Sandbox, SandboxStatus


def _make_sandbox(metadata: Optional[Dict[str, str]] = None) -> Sandbox:
    now = datetime.now(timezone.utc)
    return Sandbox(
        id="sbx-001",
        image=ImageSpec(uri="python:3.11"),
        status=SandboxStatus(state="Running"),
        metadata=metadata,
        entrypoint=["python", "-V"],
        expiresAt=now + timedelta(hours=1),
        createdAt=now,
    )


def _stub_get_sandbox(sandbox_id: str):
    from opensandbox_server.api.schema import ImageSpec, SandboxStatus
    from datetime import datetime, timedelta, timezone
    now = datetime.now(timezone.utc)
    return {"id": sandbox_id, "metadata": {}}


class TestPatchMetadataRoute:

    def test_add_keys(self, client: TestClient, auth_headers: dict, monkeypatch) -> None:
        sandbox = _make_sandbox({"team": "old"})

        class StubService:
            get_sandbox = staticmethod(_stub_get_sandbox)

            @staticmethod
            def patch_sandbox_metadata(sandbox_id: str, patch: dict) -> Sandbox:
                assert sandbox_id == "sbx-001"
                assert patch == {"project": "new-project", "version": "2.0"}
                return sandbox

        monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

        resp = client.patch(
            "/v1/sandboxes/sbx-001/metadata",
            json={"project": "new-project", "version": "2.0"},
            headers=auth_headers,
        )
        assert resp.status_code == 200

    def test_delete_key(self, client: TestClient, auth_headers: dict, monkeypatch) -> None:
        sandbox = _make_sandbox({"team": "platform"})

        class StubService:
            get_sandbox = staticmethod(_stub_get_sandbox)

            @staticmethod
            def patch_sandbox_metadata(sandbox_id: str, patch: dict) -> Sandbox:
                assert patch == {"deprecated-key": None}
                return sandbox

        monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

        resp = client.patch(
            "/v1/sandboxes/sbx-001/metadata",
            json={"deprecated-key": None},
            headers=auth_headers,
        )
        assert resp.status_code == 200

    def test_mixed_operations(self, client: TestClient, auth_headers: dict, monkeypatch) -> None:
        sandbox = _make_sandbox()

        class StubService:
            get_sandbox = staticmethod(_stub_get_sandbox)

            @staticmethod
            def patch_sandbox_metadata(sandbox_id: str, patch: dict) -> Sandbox:
                assert patch == {"project": "new", "team": None, "env": "production"}
                return sandbox

        monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

        resp = client.patch(
            "/v1/sandboxes/sbx-001/metadata",
            json={"project": "new", "team": None, "env": "production"},
            headers=auth_headers,
        )
        assert resp.status_code == 200

    def test_empty_body_noop(self, client: TestClient, auth_headers: dict, monkeypatch) -> None:
        sandbox = _make_sandbox({"team": "platform"})

        class StubService:
            get_sandbox = staticmethod(_stub_get_sandbox)

            @staticmethod
            def patch_sandbox_metadata(sandbox_id: str, patch: dict) -> Sandbox:
                assert patch == {}
                return sandbox

        monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

        resp = client.patch(
            "/v1/sandboxes/sbx-001/metadata",
            json={},
            headers=auth_headers,
        )
        assert resp.status_code == 200

    def test_not_found(self, client: TestClient, auth_headers: dict, monkeypatch) -> None:
        class StubService:
            @staticmethod
            def get_sandbox(sandbox_id: str):
                raise HTTPException(
                    status_code=404,
                    detail={"code": "SANDBOX_NOT_FOUND", "message": f"Sandbox {sandbox_id} not found"},
                )

            @staticmethod
            def patch_sandbox_metadata(sandbox_id: str, patch: dict) -> Sandbox:
                raise HTTPException(
                    status_code=404,
                    detail={"code": "SANDBOX_NOT_FOUND", "message": f"Sandbox {sandbox_id} not found"},
                )

        monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

        resp = client.patch(
            "/v1/sandboxes/missing-id/metadata",
            json={"key": "value"},
            headers=auth_headers,
        )
        assert resp.status_code == 404

    def test_invalid_metadata_rejected(self, client: TestClient, auth_headers: dict, monkeypatch) -> None:
        class StubService:
            get_sandbox = staticmethod(_stub_get_sandbox)

            @staticmethod
            def patch_sandbox_metadata(sandbox_id: str, patch: dict) -> Sandbox:
                raise HTTPException(
                    status_code=400,
                    detail={
                        "code": "SANDBOX::INVALID_METADATA_LABEL",
                        "message": "Metadata key 'opensandbox.io/foo' uses the reserved prefix",
                    },
                )

        monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

        resp = client.patch(
            "/v1/sandboxes/sbx-001/metadata",
            json={"opensandbox.io/foo": "bar"},
            headers=auth_headers,
        )
        assert resp.status_code == 400
        assert resp.json()["code"] == "SANDBOX::INVALID_METADATA_LABEL"
