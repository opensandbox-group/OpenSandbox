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
from datetime import datetime, timezone
from fastapi.testclient import TestClient

import main
from store import _normalize


class FakeStore:
    def __init__(self):
        self.events = []
        self.fail = False
        self.deleted_ids = set()
        self.unaccessed = {}  # sandbox_id -> created_at
        # created_at per known row (event rows start as None); mimics the
        # backfill in upsert_discovered_sandboxes.
        self.created_at = {}
        self.node_ips = {}  # sandbox_id -> node IP

    def init_schema(self):
        pass

    def record(self, events):
        if self.fail:
            raise RuntimeError("db down")
        self.events.extend(_normalize(event) for event in events)
        # Mimic the upsert: a first request flips the row to accessed.
        for event in events:
            self.created_at.setdefault(event["sandbox_id"], None)
            self.unaccessed.pop(event["sandbox_id"], None)
        return len(events)

    def upsert_discovered_sandboxes(self, sandboxes):
        if self.fail:
            raise RuntimeError("db down")
        discovered = [
            sandbox
            for sandbox in sandboxes
            if sandbox["name"] not in self.created_at
        ]
        backfilled = [
            sandbox
            for sandbox in sandboxes
            if sandbox["name"] in self.created_at
            and self.created_at[sandbox["name"]] is None
            and sandbox.get("created_at") is not None
        ]
        for sandbox in discovered:
            self.created_at[sandbox["name"]] = sandbox.get("created_at")
            self.unaccessed[sandbox["name"]] = sandbox.get("created_at")
        for sandbox in backfilled:
            self.created_at[sandbox["name"]] = sandbox["created_at"]
        return {"discovered": len(discovered), "backfilled": len(backfilled)}

    def update_node_ips(self, nodes):
        if self.fail:
            raise RuntimeError("db down")
        known = set(self.created_at) | set(self.unaccessed)
        changed = [
            sandbox_id
            for sandbox_id, ip in nodes.items()
            if sandbox_id in known and self.node_ips.get(sandbox_id) != ip
        ]
        for sandbox_id in changed:
            self.node_ips[sandbox_id] = nodes[sandbox_id]
        return len(changed)

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

        def matches_search(sandbox_id):
            if search is None:
                return True
            # Mimic the SQL: fuzzy on sandbox_id OR exact on node_ip.
            return (
                search.lower() in sandbox_id.lower()
                or self.node_ips.get(sandbox_id) == search
            )

        items = [
            {
                "sandbox_id": event["sandbox_id"],
                "uri": event["uri"],
                "method": event["method"],
                "target": event["target"],
                "request_time": event["request_time"],
                "request_count": 1,
                "accessed": True,
                "created_at": self.created_at.get(event["sandbox_id"]),
                "node_ip": self.node_ips.get(event["sandbox_id"]),
            }
            for event in self.events
            if event["sandbox_id"] not in self.deleted_ids
            and matches_search(event["sandbox_id"])
            and (time_from is None or time_from <= event["request_time"])
            and (time_to is None or event["request_time"] <= time_to)
        ]
        # Discovered-but-never-accessed rows: NULL request fields, count 0.
        # Time filters exclude them (NULL comparisons are not TRUE in SQL).
        if time_from is None and time_to is None:
            items.extend(
                {
                    "sandbox_id": sandbox_id,
                    "uri": None,
                    "method": None,
                    "target": None,
                    "request_time": None,
                    "request_count": 0,
                    "accessed": False,
                    "created_at": created_at,
                    "node_ip": self.node_ips.get(sandbox_id),
                }
                for sandbox_id, created_at in self.unaccessed.items()
                if sandbox_id not in self.deleted_ids
                and matches_search(sandbox_id)
            )
        return {
            "total": len(items),
            "items": items[offset : offset + limit],
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
def client(monkeypatch):
    fake = FakeStore()
    # Force auth off regardless of the local audit.toml so tests are
    # independent of the developer's configuration (auth-specific tests
    # override this via monkeypatch themselves).
    monkeypatch.setattr(main, "UI_PASSWORD", "")
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

    # Sort by accessed (ascending = never-accessed first) is allowed.
    response = test_client.get("/api/sandboxes", params={"sort": "accessed"})
    assert response.status_code == 200
    assert response.json()["sort"] == "accessed"

    # Sort by created_at (sandbox creation timestamp) is allowed.
    response = test_client.get("/api/sandboxes", params={"sort": "-created_at"})
    assert response.status_code == 200
    assert response.json()["sort"] == "-created_at"

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
    def __init__(self, names, created_at=None, pod_nodes=None):
        self.names = names
        self.created_at = created_at
        # sandbox_id -> node IP; defaults to none (no pods listed).
        self.pod_nodes = pod_nodes or {}
        self.calls = 0

    def list_batch_sandboxes(self, namespace):
        self.calls += 1
        return [{"name": name, "created_at": self.created_at} for name in self.names]

    def list_sandbox_pod_nodes(self, namespace):
        self.calls += 1
        return dict(self.pod_nodes)


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
        def list_batch_sandboxes(self, namespace):
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


def test_sync_discovers_unaccessed_sandboxes(monkeypatch, client):
    """Cluster sandbox ids missing from the database are inserted as unaccessed rows."""
    monkeypatch.setattr(main, "K8S_NAMESPACE", "opensandbox")
    test_client, fake = client
    fake.record([EVENT])
    created = datetime(2026, 8, 30, 4, 5, 6, tzinfo=timezone.utc)
    fake_k8s = FakeK8sClient(["test-sandbox", "fresh"], created_at=created)
    monkeypatch.setattr(main.k8s, "K8sClient", lambda _: fake_k8s)
    main.app.state.k8s_client = fake_k8s
    try:
        response = test_client.post("/api/sync-deleted")
    finally:
        main.app.state.k8s_client = None

    assert response.status_code == 200
    data = response.json()
    assert data["discovered"] == 1
    # "test-sandbox" already had a row without created_at -> backfilled.
    assert data["backfilled"] == 1
    assert data["deleted"] == 0

    # The discovered sandbox shows up in the summary API as never accessed,
    # carrying the BatchSandbox creationTimestamp.
    listing = test_client.get("/api/sandboxes")
    assert listing.status_code == 200
    items = {item["sandbox_id"]: item for item in listing.json()["items"]}
    assert set(items) == {"test-sandbox", "fresh"}
    assert items["fresh"]["accessed"] is False
    assert items["fresh"]["request_count"] == 0
    assert items["fresh"]["uri"] is None
    assert items["fresh"]["request_time"] is None
    assert items["fresh"]["created_at"] == "2026-08-30T04:05:06Z"
    assert items["test-sandbox"]["accessed"] is True
    # Backfilled from the sync (discovery happened mid-test).
    assert items["test-sandbox"]["created_at"] == "2026-08-30T04:05:06Z"

    # A second sync discovers nothing new.
    main.app.state.k8s_client = fake_k8s
    try:
        assert test_client.post("/api/sync-deleted").json()["discovered"] == 0
    finally:
        main.app.state.k8s_client = None


def test_sync_backfills_creation_timestamp(monkeypatch, client):
    """Existing rows missing created_at get it backfilled from the resource."""
    monkeypatch.setattr(main, "K8S_NAMESPACE", "opensandbox")
    test_client, fake = client
    # The sandbox was accessed before the first sync ran, so its row
    # exists but has no creation timestamp.
    fake.record([EVENT])
    created = datetime(2026, 8, 19, 8, 30, 0, tzinfo=timezone.utc)
    fake_k8s = FakeK8sClient(["test-sandbox"], created_at=created)
    monkeypatch.setattr(main.k8s, "K8sClient", lambda _: fake_k8s)
    main.app.state.k8s_client = fake_k8s
    try:
        response = test_client.post("/api/sync-deleted")
    finally:
        main.app.state.k8s_client = None

    assert response.status_code == 200
    data = response.json()
    assert data["discovered"] == 0
    assert data["backfilled"] == 1

    items = {
        item["sandbox_id"]: item
        for item in test_client.get("/api/sandboxes").json()["items"]
    }
    assert items["test-sandbox"]["created_at"] == "2026-08-19T08:30:00Z"
    assert items["test-sandbox"]["accessed"] is True
    assert items["test-sandbox"]["uri"] == EVENT["uri"]

    # Re-syncing with the same timestamp backfills nothing more.
    main.app.state.k8s_client = fake_k8s
    try:
        assert test_client.post("/api/sync-deleted").json()["backfilled"] == 0
    finally:
        main.app.state.k8s_client = None


def test_sync_updates_node_ips(monkeypatch, client):
    """The sync refreshes node_ip from the sandbox pods' host IPs."""
    monkeypatch.setattr(main, "K8S_NAMESPACE", "opensandbox")
    test_client, fake = client
    fake.record([EVENT, {**EVENT, "sandbox_id": "other"}])
    fake_k8s = FakeK8sClient(
        ["test-sandbox", "other"],
        pod_nodes={"test-sandbox": "10.0.0.1", "other": "10.0.0.2"},
    )
    monkeypatch.setattr(main.k8s, "K8sClient", lambda _: fake_k8s)
    main.app.state.k8s_client = fake_k8s
    try:
        response = test_client.post("/api/sync-deleted")
    finally:
        main.app.state.k8s_client = None

    assert response.status_code == 200
    data = response.json()
    assert data["node_updated"] == 2

    items = {
        item["sandbox_id"]: item
        for item in test_client.get("/api/sandboxes").json()["items"]
    }
    assert items["test-sandbox"]["node_ip"] == "10.0.0.1"
    assert items["other"]["node_ip"] == "10.0.0.2"

    # Searching by node IP returns every sandbox on that node.
    listing = test_client.get("/api/sandboxes", params={"search": "10.0.0.1"})
    assert listing.status_code == 200
    assert listing.json()["total"] == 1
    assert listing.json()["items"][0]["sandbox_id"] == "test-sandbox"

    # A rescheduled pod (new host IP) overwrites the old value; unchanged
    # rows are not counted again.
    fake_k8s.pod_nodes = {"test-sandbox": "10.0.0.9", "other": "10.0.0.2"}
    main.app.state.k8s_client = fake_k8s
    try:
        response = test_client.post("/api/sync-deleted")
    finally:
        main.app.state.k8s_client = None
    assert response.json()["node_updated"] == 1
    items = {
        item["sandbox_id"]: item
        for item in test_client.get("/api/sandboxes").json()["items"]
    }
    assert items["test-sandbox"]["node_ip"] == "10.0.0.9"


def test_search_by_ip_finds_unaccessed_sandboxes(monkeypatch, client):
    """IP search also matches discovered-but-never-accessed rows."""
    monkeypatch.setattr(main, "K8S_NAMESPACE", "opensandbox")
    test_client, fake = client
    fake.upsert_discovered_sandboxes([{"name": "fresh", "created_at": None}])
    fake.update_node_ips({"fresh": "10.0.0.5"})

    listing = test_client.get("/api/sandboxes", params={"search": "10.0.0.5"})

    assert listing.status_code == 200
    assert listing.json()["total"] == 1
    assert listing.json()["items"][0]["sandbox_id"] == "fresh"
    assert listing.json()["items"][0]["node_ip"] == "10.0.0.5"


def test_discovered_sandbox_becomes_accessed_on_first_event(monkeypatch, client):
    """The first audit event for a discovered sandbox flips it to accessed."""
    monkeypatch.setattr(main, "K8S_NAMESPACE", "opensandbox")
    test_client, fake = client
    fake.upsert_discovered_sandboxes([{"name": "fresh", "created_at": None}])

    items = {
        item["sandbox_id"]: item
        for item in test_client.get("/api/sandboxes").json()["items"]
    }
    assert items["fresh"]["accessed"] is False

    test_client.post("/events", json={**EVENT, "sandbox_id": "fresh"})

    items = {
        item["sandbox_id"]: item
        for item in test_client.get("/api/sandboxes").json()["items"]
    }
    assert items["fresh"]["accessed"] is True
    assert items["fresh"]["uri"] == EVENT["uri"]
