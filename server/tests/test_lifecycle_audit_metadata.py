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

from opensandbox_server.api.schema import (
    CreateSandboxRequest,
    CreateSandboxResponse,
    ImageSpec,
    ResourceLimits,
    SandboxStatus,
)
from opensandbox_server.services.lifecycle_audit import (
    _audit_metadata,
    _parse_memory_gi,
    load_sandbox_for_audit_delete,
    publish_lifecycle_audit_shape,
    schedule_lifecycle_audit_create,
    shape_from_pool_manifest,
    shape_from_template_spec,
)


def test_audit_metadata_keeps_label_map() -> None:
    assert _audit_metadata({"env": "prod", " app ": "zpert"}) == {"env": "prod", "app": "zpert"}


def test_audit_metadata_drops_empty() -> None:
    assert _audit_metadata(None) is None
    assert _audit_metadata({}) is None
    assert _audit_metadata({" ": "x"}) is None


def test_bare_memory_quantity_is_bytes() -> None:
    assert _parse_memory_gi("1073741824") == 1
    assert _parse_memory_gi("512Mi") == 0.5


def test_delete_lookup_is_skipped_when_audit_is_disabled(monkeypatch) -> None:
    from opensandbox_server.services import lifecycle_audit as audit_module

    monkeypatch.setattr(audit_module, "lifecycle_audit_enabled", lambda: False)
    called = False

    def load():
        nonlocal called
        called = True
        raise AssertionError("get_sandbox should not run")

    assert load_sandbox_for_audit_delete(load) is None
    assert called is False


def test_pool_manifest_shape_uses_the_sandbox_container() -> None:
    shape = shape_from_pool_manifest(
        {
            "spec": {
                "template": {
                    "spec": {
                        "containers": [
                            {"name": "execd", "image": "execd:1", "resources": {"limits": {"cpu": "100m"}}},
                            {
                                "name": "sandbox",
                                "image": "python:3.11",
                                "resources": {"limits": {"cpu": "2", "memory": "1Gi"}},
                            },
                        ]
                    }
                }
            }
        }
    )
    assert shape.image_uri == "python:3.11"
    assert shape.resource_limits is not None
    assert shape.resource_limits.root == {"cpu": "2", "memory": "1Gi"}


def test_template_shape_uses_source_image_and_defaults() -> None:
    shape = shape_from_template_spec("ubuntu:22.04", None)
    assert shape.image_uri == "ubuntu:22.04"
    assert shape.resource_limits is not None
    assert shape.resource_limits.root["cpu"] == "1"
    assert shape.resource_limits.root["memory"] == "512Mi"


def test_schedule_create_records_published_shape(monkeypatch) -> None:
    from opensandbox_server.services import lifecycle_audit as audit_module

    recorded: dict = {}

    class _Repo:
        def upsert_created(self, **kwargs: object) -> None:
            recorded.update(kwargs)

    class _Tasks:
        def __init__(self) -> None:
            self.calls: list = []

        def add_task(self, func, **kwargs) -> None:  # type: ignore[no-untyped-def]
            self.calls.append((func, kwargs))

    monkeypatch.setattr(audit_module, "lifecycle_audit_enabled", lambda: True)
    monkeypatch.setattr(audit_module, "get_lifecycle_audit_repository", lambda: _Repo())
    monkeypatch.setattr(audit_module, "_tenant_context", lambda: ("acme", "ns-acme"))
    publish_lifecycle_audit_shape(
        image_uri="pool.example/sandbox:1",
        resource_limits=ResourceLimits(root={"cpu": "2", "memory": "1073741824"}),
    )
    request = CreateSandboxRequest(
        image=ImageSpec(uri="caller.example/ignored:1"),
        entrypoint=["sleep"],
        timeout=60,
        resourceLimits=ResourceLimits(root={"cpu": "100m", "memory": "128Mi"}),
        extensions={"poolRef": "pool-a"},
    )
    response = CreateSandboxResponse(
        id="sbx-pool",
        status=SandboxStatus(state="Pending"),
        createdAt=datetime(2026, 1, 1, tzinfo=timezone.utc),
    )
    tasks = _Tasks()
    schedule_lifecycle_audit_create(tasks, request, response)  # type: ignore[arg-type]
    tasks.calls[0][0](**tasks.calls[0][1])
    assert recorded["image_uri"] == "pool.example/sandbox:1"
    assert recorded["cpu_limit"] == "2"
    assert recorded["memory_gi"] == 1
