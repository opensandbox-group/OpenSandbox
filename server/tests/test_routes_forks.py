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

from datetime import datetime, timezone
from unittest.mock import AsyncMock, Mock

from opensandbox_server.api import lifecycle
from opensandbox_server.api.fork_schema import ForkOperation


def operation():
    now = datetime.now(timezone.utc)
    return ForkOperation.model_validate({"id": "fork-1", "sourceSandboxId": "source",
                                         "status": {"state": "Pending"},
                                         "createdAt": now, "updatedAt": now})


def test_fork_returns_location_and_forwards_key(client, auth_headers, monkeypatch):
    service = Mock()
    service.fork = AsyncMock(return_value=operation())
    monkeypatch.setattr(lifecycle, "fork_service", service)
    response = client.post("/v1/sandboxes/source/fork", json={"timeout": 1800},
                           headers={**auth_headers, "Idempotency-Key": "retry"})
    assert response.status_code == 202
    assert response.headers["location"] == "/v1/forks/fork-1"
    assert response.json()["status"]["state"] == "Pending"
    assert "sandboxId" not in response.json()
    assert service.fork.call_args.args[2] == "retry"


def test_get_fork_returns_operation(client, auth_headers, monkeypatch):
    service = Mock()
    service.get.return_value = operation()
    monkeypatch.setattr(lifecycle, "fork_service", service)
    response = client.get("/v1/forks/fork-1", headers=auth_headers)
    assert response.status_code == 200
    assert response.json()["sourceSandboxId"] == "source"
    service.get.assert_called_once_with("fork-1")


def test_fork_rejects_batch_and_missing_timeout(client, auth_headers):
    for body in ({}, {"timeout": 1800, "count": 3}, {"timeout": 1800, "overrides": {"env": None}}):
        assert client.post("/v1/sandboxes/source/fork", json=body, headers=auth_headers).status_code == 422
