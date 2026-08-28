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

"""Tests for the audit webhook API.

Uses a fake store so no PostgreSQL instance is required.
"""

import pytest
from fastapi.testclient import TestClient

import main
from store import _normalize


class FakeStore:
    def __init__(self):
        self.events = []
        self.fail = False
        self.deleted_ids = set()

    def init_schema(self):
        pass

    def record(self, events):
        if self.fail:
            raise RuntimeError("db down")
        self.events.extend(_normalize(event) for event in events)
        return len(events)

    def list_latest(
        self,
        search=None,
        sort="-request_time",
        limit=50,
        offset=0,
        time_from=None,
        time_to=None,
    ):
        if self.fail:
            raise RuntimeError("db down")
        events = [
            event for event in self.events
            if event["sandbox_id"] not in self.deleted_ids
            and (search is None or search.lower() in event["sandbox_id"].lower())
            and (time_from is None or time_from <= event["request_time"])
            and (time_to is None or event["request_time"] <= time_to)
        ]
        return {
            "total": len(events),
            "items": [
                {
                    "sandbox_id": event["sandbox_id"],
                    "uri": event["uri"],
                    "method": event["method"],
                    "target": event["target"],
                    "request_time": event["request_time"],
                    "request_count": 1,
                }
                for event in events[offset : offset + limit]
            ],
            "sort": sort,
            "search": search,
        }

    def list_details(self, sandbox_id=None, limit=50, offset=0):
        if self.fail:
            raise RuntimeError("db down")
        matches = [
            event for event in self.events if sandbox_id is None or event["sandbox_id"] == sandbox_id
        ]
        items = [
            {"id": index + 1, "received_at": event["request_time"], **event}
            for index, event in enumerate(matches[offset : offset + limit])
        ]
        return {"total": len(matches), "items": items}

    def sync_deleted_flags(self, live_ids):
        if self.fail:
            raise RuntimeError("db down")
        live = set(live_ids)
        all_ids = {event["sandbox_id"] for event in self.events}
        prev_deleted = getattr(self, "deleted_ids", set())
        # Mimic the SQL: only rows whose flag actually changes are counted.
        deleted = (all_ids - prev_deleted) - live
        restored = prev_deleted & live
        self.deleted_ids = (prev_deleted | all_ids) - live
        return {"deleted": len(deleted), "restored": len(restored)}


@pytest.fixture()
def client():
    fake = FakeStore()
    # Bypass the lifespan (which opens a real PostgreSQL pool) by injecting
    # the fake store directly into app state.
    main.app.state.store = fake
    test_client = TestClient(main.app)
    yield test_client, fake
    main.app.state.store = None


EVENT = {
    "sandbox_id": "test-sandbox",
    "uri": "/api/users",
    "method": "GET",
    "target": "10.0.0.1:8080",
    "request_time": "2026-08-20T09:24:12.252+08:00",
}


def test_record_single_event(client):
    test_client, fake = client

    response = test_client.post("/events", json=EVENT)

    assert response.status_code == 200
    assert response.json() == {"accepted": 1}
    assert len(fake.events) == 1
    assert fake.events[0]["sandbox_id"] == "test-sandbox"
    # Timestamps are normalized (UTC-assumed if naive, truncated to seconds).
    assert fake.events[0]["request_time"].isoformat() == "2026-08-20T09:24:12+08:00"


def test_record_event_batch(client):
    test_client, fake = client

    response = test_client.post("/events", json=[EVENT, {**EVENT, "sandbox_id": "other"}])

    assert response.status_code == 200
    assert response.json() == {"accepted": 2}
    assert len(fake.events) == 2


def test_record_event_at_root_alias(client):
    """POST / is accepted as an alias for POST /events (path-less webhook URLs)."""
    test_client, fake = client

    response = test_client.post("/", json=EVENT)

    assert response.status_code == 200
    assert response.json() == {"accepted": 1}
    assert len(fake.events) == 1


def test_ping_requests_not_recorded(client):
    """Requests whose URI ends with 'ping' (case-insensitive) are dropped."""
    test_client, fake = client

    # Single ping event: accepted as 0, nothing stored.
    response = test_client.post(
        "/events",
        json={**EVENT, "uri": "/610c205a-272e-425f-85bf-b27cae2d9ee3/44772/ping"},
    )
    assert response.status_code == 200
    assert response.json() == {"accepted": 0}
    assert fake.events == []

    # Mixed batch: only URIs not ending with 'ping' are recorded; 'ping'
    # elsewhere in the URI (not at the end) is kept.
    response = test_client.post(
        "/events",
        json=[
            EVENT,
            {**EVENT, "uri": "/some-sandbox/8080/PING"},
            {**EVENT, "uri": "/api/users?watch=ping&since=1"},
        ],
    )
    assert response.status_code == 200
    assert response.json() == {"accepted": 2}
    assert all(
        not event["uri"].lower().endswith("ping") for event in fake.events
    )
    assert any(
        event["uri"] == "/api/users?watch=ping&since=1" for event in fake.events
    )


def test_reject_invalid_event(client):
    test_client, fake = client

    missing_field = {k: v for k, v in EVENT.items() if k != "target"}
    response = test_client.post("/events", json=missing_field)

    assert response.status_code == 422
    assert fake.events == []


def test_store_failure_returns_502(client):
    test_client, fake = client
    fake.fail = True

    response = test_client.post("/events", json=EVENT)

    assert response.status_code == 502


def test_healthz(client):
    test_client, _ = client

    response = test_client.get("/status.ok")

    assert response.status_code == 200
    assert response.json() == {"status": "ok"}


def test_index_page(client):
    test_client, _ = client

    response = test_client.get("/")

    assert response.status_code == 200
    assert "html" in response.headers["content-type"]
    # The page is a React app built from frontend/; the title lives in the
    # built HTML shell and the table renders client-side.
    assert "沙箱最新请求" in response.text
    assert "/static/assets/" in response.text


def test_details_page(client):
    test_client, _ = client

    response = test_client.get("/details")

    assert response.status_code == 200
    assert "html" in response.headers["content-type"]
    assert "请求详情" in response.text


def test_login_page(client):
    test_client, _ = client

    response = test_client.get("/login")

    assert response.status_code == 200
    assert "html" in response.headers["content-type"]
    assert "密码" in response.text


def test_auth_flow(monkeypatch, client):
    """When AUDIT_UI_PASSWORD is set, pages/APIs require login; events stay open."""
    monkeypatch.setattr(main, "UI_PASSWORD", "secret")
    test_client, _ = client

    # Pages redirect to /login without a session.
    for path in ("/", "/details"):
        response = test_client.get(path, follow_redirects=False)
        assert response.status_code == 303
        assert response.headers["location"] == "/login"

    # APIs reject with 401.
    assert test_client.get("/api/sandboxes").status_code == 401
    assert test_client.get("/api/requests").status_code == 401

    # Event ingestion is never password protected.
    assert test_client.post("/events", json=EVENT).status_code == 200

    # Wrong password is rejected.
    assert test_client.post("/login", json={"password": "wrong"}).status_code == 401

    # Correct password issues a session cookie and unlocks pages/APIs.
    response = test_client.post("/login", json={"password": "secret"})
    assert response.status_code == 200
    assert test_client.get("/", follow_redirects=False).status_code == 200
    assert test_client.get("/details", follow_redirects=False).status_code == 200
    assert test_client.get("/api/sandboxes").status_code == 200
    assert test_client.get("/api/requests").status_code == 200

    # Logout invalidates the session.
    assert test_client.post("/logout").status_code == 200
    assert test_client.get("/api/sandboxes").status_code == 401


def test_auth_disabled_by_default(client):
    """Without AUDIT_UI_PASSWORD, pages/APIs are open and login is a no-op."""
    test_client, _ = client

    assert test_client.get("/", follow_redirects=False).status_code == 200
    assert test_client.get("/api/sandboxes").status_code == 200
    assert test_client.post("/login", json={"password": "anything"}).status_code == 200


def test_list_sandboxes(client):
    test_client, fake = client
    fake.record([EVENT, {**EVENT, "sandbox_id": "other"}])

    response = test_client.get("/api/sandboxes")

    assert response.status_code == 200
    data = response.json()
    assert data["total"] == 2
    assert len(data["items"]) == 2
    assert data["items"][0]["sandbox_id"] == "test-sandbox"


def test_list_sandboxes_fuzzy_search(client):
    test_client, fake = client
    fake.record([EVENT, {**EVENT, "sandbox_id": "other-sandbox"}])

    response = test_client.get("/api/sandboxes", params={"search": "TEST"})

    assert response.status_code == 200
    data = response.json()
    assert data["total"] == 1
    assert data["items"][0]["sandbox_id"] == "test-sandbox"


def test_list_sandboxes_sort(client):
    test_client, _ = client

    # Ascending order is passed through.
    response = test_client.get("/api/sandboxes", params={"sort": "request_time"})
    assert response.status_code == 200
    assert response.json()["sort"] == "request_time"

    # Sort by request_count (descending) is allowed.
    response = test_client.get("/api/sandboxes", params={"sort": "-request_count"})
    assert response.status_code == 200
    assert response.json()["sort"] == "-request_count"

    # Unknown sort keys are rejected.
    response = test_client.get("/api/sandboxes", params={"sort": "sandbox_id"})
    assert response.status_code == 422


def test_list_sandboxes_time_range_filter(client):
    """time_from/time_to filter sandboxes by latest request time."""
    test_client, fake = client
    # EVENT's request_time is 2026-08-20T09:24:12+08:00 (= 01:24:12 UTC).
    fake.record([
        EVENT,
        {**EVENT, "sandbox_id": "older", "request_time": "2026-08-10T09:24:12.252+08:00"},
        {**EVENT, "sandbox_id": "newer", "request_time": "2026-08-21T09:24:12.252+08:00"},
    ])

    # Only sandboxes whose latest request falls in the range are returned.
    response = test_client.get(
        "/api/sandboxes",
        params={"time_from": "2026-08-20T00:00:00Z", "time_to": "2026-08-20T23:59:59Z"},
    )

    assert response.status_code == 200
    data = response.json()
    assert data["total"] == 1
    assert data["items"][0]["sandbox_id"] == "test-sandbox"

    # The bounds are inclusive and can be used alone.
    response = test_client.get(
        "/api/sandboxes", params={"time_from": "2026-08-21T01:24:12Z"}
    )
    assert response.json()["total"] == 1
    assert response.json()["items"][0]["sandbox_id"] == "newer"


def test_list_sandboxes_rejects_invalid_time_range(client):
    test_client, _ = client

    response = test_client.get(
        "/api/sandboxes", params={"time_from": "not-a-timestamp"}
    )

    assert response.status_code == 422


def test_list_requests_with_filter(client):
    test_client, fake = client
    fake.record([EVENT, {**EVENT, "sandbox_id": "other"}])

    response = test_client.get("/api/requests", params={"sandbox_id": "test-sandbox"})

    assert response.status_code == 200
    data = response.json()
    assert data["total"] == 1
    assert data["items"][0]["sandbox_id"] == "test-sandbox"


def test_list_requests_pagination(client):
    test_client, fake = client
    fake.record([{**EVENT, "sandbox_id": f"sandbox-{i}"} for i in range(5)])

    response = test_client.get("/api/requests", params={"limit": 2, "offset": 3})

    assert response.status_code == 200
    data = response.json()
    assert data["total"] == 5
    assert len(data["items"]) == 2


def test_list_requests_rejects_invalid_pagination(client):
    test_client, _ = client

    response = test_client.get("/api/requests", params={"limit": 0})

    assert response.status_code == 422


def test_list_query_failure_returns_502(client):
    test_client, fake = client
    fake.fail = True

    assert test_client.get("/api/sandboxes").status_code == 502
    assert test_client.get("/api/requests").status_code == 502


def test_normalize_parses_rfc3339_zulu():
    event = _normalize({**EVENT, "request_time": "2026-08-20T01:24:12.252Z"})

    assert event["request_time"].utcoffset().total_seconds() == 0


def test_normalize_assumes_utc_for_naive_timestamp():
    event = _normalize({**EVENT, "request_time": "2026-08-20T01:24:12"})

    assert event["request_time"].utcoffset().total_seconds() == 0


def test_normalize_truncates_to_seconds():
    event = _normalize({**EVENT, "request_time": "2026-08-20T09:24:12.252953+08:00"})

    assert event["request_time"].isoformat() == "2026-08-20T09:24:12+08:00"


class FakeK8sClient:
    def __init__(self, names):
        self.names = names
        self.calls = 0

    def list_batch_sandbox_names(self, namespace):
        self.calls += 1
        return list(self.names)


def test_sync_deleted_marks_missing_sandboxes(monkeypatch, client):
    """Sandbox ids absent from the k8s resource names are marked deleted."""
    monkeypatch.setattr(main, "K8S_NAMESPACE", "opensandbox")
    test_client, fake = client
    fake.record([EVENT, {**EVENT, "sandbox_id": "other"}])
    fake_k8s = FakeK8sClient(["test-sandbox"])
    monkeypatch.setattr(main.k8s, "K8sClient", lambda _: fake_k8s)
    main.app.state.k8s_client = fake_k8s
    try:
        response = test_client.post("/api/sync-deleted")
    finally:
        main.app.state.k8s_client = None

    assert response.status_code == 200
    data = response.json()
    assert data["namespace"] == "opensandbox"
    assert data["live"] == 1
    assert data["deleted"] == 1
    assert data["restored"] == 0
    assert fake.deleted_ids == {"other"}

    # Deleted sandboxes are hidden from the summary API (and thus the UI).
    listing = test_client.get("/api/sandboxes")
    assert listing.status_code == 200
    assert listing.json()["total"] == 1
    assert listing.json()["items"][0]["sandbox_id"] == "test-sandbox"


def test_sync_deleted_restores_reappeared_sandbox(monkeypatch, client):
    """A sandbox id that reappears in the cluster is un-deleted."""
    monkeypatch.setattr(main, "K8S_NAMESPACE", "opensandbox")
    test_client, fake = client
    fake.record([EVENT, {**EVENT, "sandbox_id": "other"}])
    fake_k8s = FakeK8sClient([])
    monkeypatch.setattr(main.k8s, "K8sClient", lambda _: fake_k8s)
    main.app.state.k8s_client = fake_k8s
    try:
        assert test_client.post("/api/sync-deleted").json()["deleted"] == 2

        fake_k8s.names = ["test-sandbox", "other"]
        response = test_client.post("/api/sync-deleted")
    finally:
        main.app.state.k8s_client = None

    assert response.status_code == 200
    assert response.json()["restored"] == 2
    assert fake.deleted_ids == set()
    assert test_client.get("/api/sandboxes").json()["total"] == 2


def test_sync_deleted_requires_namespace(monkeypatch, client):
    """Without AUDIT_K8S_NAMESPACE the sync endpoint rejects with 400."""
    monkeypatch.setattr(main, "K8S_NAMESPACE", "")
    test_client, _ = client

    assert test_client.post("/api/sync-deleted").status_code == 400


def test_sync_deleted_k8s_failure_returns_502(monkeypatch, client):
    monkeypatch.setattr(main, "K8S_NAMESPACE", "opensandbox")
    test_client, _ = client

    class BrokenK8sClient:
        def list_batch_sandbox_names(self, namespace):
            raise main.k8s.K8sError("api down")

    monkeypatch.setattr(main.k8s, "K8sClient", lambda _: BrokenK8sClient())
    main.app.state.k8s_client = None
    response = test_client.post("/api/sync-deleted")

    assert response.status_code == 502


def test_sync_deleted_requires_auth(monkeypatch, client):
    monkeypatch.setattr(main, "K8S_NAMESPACE", "opensandbox")
    monkeypatch.setattr(main, "UI_PASSWORD", "secret")
    test_client, _ = client

    assert test_client.post("/api/sync-deleted").status_code == 401
