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

import time
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timedelta, timezone

from fastapi.testclient import TestClient

from opensandbox_server.api import lifecycle
from opensandbox_server.api.schema import (
    ImageSpec,
    ListSandboxesResponse,
    PaginationInfo,
    Sandbox,
    SandboxStatus,
)


def test_list_sandboxes_parses_filters_and_pagination(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)
    captured_requests: list[object] = []

    class StubService:
        @staticmethod
        def list_sandboxes(request) -> ListSandboxesResponse:
            captured_requests.append(request)
            return ListSandboxesResponse(
                items=[
                    Sandbox(
                        id="sbx-001",
                        image=ImageSpec(uri="python:3.11"),
                        status=SandboxStatus(state="Running"),
                        metadata={"team": "infra", "project": "alpha"},
                        entrypoint=["python", "-V"],
                        expiresAt=now + timedelta(hours=1),
                        createdAt=now,
                    )
                ],
                pagination=PaginationInfo(
                    page=2,
                    pageSize=5,
                    totalItems=8,
                    totalPages=2,
                    hasNextPage=False,
                ),
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes",
        params={
            "state": ["Running", "Paused"],
            "metadata": "team=infra&project=alpha",
            "page": 2,
            "pageSize": 5,
        },
        headers=auth_headers,
    )

    assert response.status_code == 200
    payload = response.json()
    assert payload["pagination"]["page"] == 2
    assert payload["pagination"]["pageSize"] == 5
    assert payload["items"][0]["status"]["state"] == "Running"
    assert captured_requests[0].filter.state == ["Running", "Paused"]
    assert captured_requests[0].filter.metadata == {"team": "infra", "project": "alpha"}
    assert captured_requests[0].pagination.page == 2
    assert captured_requests[0].pagination.page_size == 5


def test_list_sandboxes_rejects_malformed_metadata_query(
    client: TestClient,
    auth_headers: dict,
) -> None:
    response = client.get(
        "/v1/sandboxes",
        params={"metadata": "team=infra&broken"},
        headers=auth_headers,
    )

    assert response.status_code == 400
    assert response.json()["code"] == "INVALID_METADATA_FORMAT"
    assert "bad query field" in response.json()["message"]


def test_list_sandboxes_keeps_blank_metadata_values(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    captured_requests: list[object] = []

    class StubService:
        @staticmethod
        def list_sandboxes(request) -> ListSandboxesResponse:
            captured_requests.append(request)
            return ListSandboxesResponse(
                items=[],
                pagination=PaginationInfo(
                    page=1,
                    pageSize=20,
                    totalItems=0,
                    totalPages=0,
                    hasNextPage=False,
                ),
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.get(
        "/v1/sandboxes",
        params={"metadata": "team=infra&note="},
        headers=auth_headers,
    )

    assert response.status_code == 200
    assert captured_requests[0].filter.metadata == {"team": "infra", "note": ""}


def test_list_sandboxes_omits_none_fields(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    now = datetime.now(timezone.utc)

    class StubService:
        @staticmethod
        def list_sandboxes(request) -> ListSandboxesResponse:
            return ListSandboxesResponse(
                items=[
                    Sandbox(
                        id="sbx-manual",
                        image=ImageSpec(uri="python:3.11"),
                        status=SandboxStatus(state="Running"),
                        metadata=None,
                        entrypoint=["python"],
                        expiresAt=None,
                        createdAt=now,
                    )
                ],
                pagination=PaginationInfo(
                    page=1,
                    pageSize=20,
                    totalItems=1,
                    totalPages=1,
                    hasNextPage=False,
                ),
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", StubService())

    response = client.get("/v1/sandboxes", headers=auth_headers)

    assert response.status_code == 200
    item = response.json()["items"][0]
    assert "expiresAt" not in item
    assert "metadata" not in item
    assert "reason" not in item["status"]
    assert "message" not in item["status"]
    assert "lastTransitionAt" not in item["status"]


def test_list_sandboxes_validates_page_bounds(
    client: TestClient,
    auth_headers: dict,
) -> None:
    response = client.get(
        "/v1/sandboxes",
        params={"page": 0},
        headers=auth_headers,
    )

    assert response.status_code == 422


def test_list_sandboxes_validates_page_size_upper_bound(
    client: TestClient,
    auth_headers: dict,
) -> None:
    response = client.get(
        "/v1/sandboxes",
        params={"pageSize": 201},
        headers=auth_headers,
    )

    assert response.status_code == 422


def test_list_sandboxes_requires_api_key(client: TestClient) -> None:
    response = client.get("/v1/sandboxes")

    assert response.status_code == 401
    assert response.json()["code"] == "MISSING_API_KEY"


def test_list_sandboxes_runs_in_threadpool_for_concurrency(
    client: TestClient,
    auth_headers: dict,
    monkeypatch,
) -> None:
    """Blocking list calls must run in the threadpool so concurrent requests
    do not serialize on the event loop. With sync def routes, FastAPI offloads
    the handler to anyio's threadpool; 8 calls each sleeping 200ms should
    complete well under the 1.6s serial bound.
    """
    sleep_seconds = 0.2
    concurrency = 8

    class SlowService:
        @staticmethod
        def list_sandboxes(_request) -> ListSandboxesResponse:
            time.sleep(sleep_seconds)
            return ListSandboxesResponse(
                items=[],
                pagination=PaginationInfo(
                    page=1,
                    pageSize=20,
                    totalItems=0,
                    totalPages=0,
                    hasNextPage=False,
                ),
            )

    monkeypatch.setattr(lifecycle, "sandbox_service", SlowService())

    def call() -> int:
        return client.get("/v1/sandboxes", headers=auth_headers).status_code

    started = time.monotonic()
    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        statuses = list(pool.map(lambda _: call(), range(concurrency)))
    elapsed = time.monotonic() - started

    assert statuses == [200] * concurrency
    serial_floor = sleep_seconds * concurrency
    assert elapsed < serial_floor * 0.6, (
        f"list_sandboxes serialized: elapsed={elapsed:.2f}s, "
        f"serial floor={serial_floor:.2f}s (threadpool offload broken)"
    )
