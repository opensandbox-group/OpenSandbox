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

"""Lifecycle activity recording (OSEP-0024): resume/create restart the clock."""

import asyncio

import pytest

from opensandbox_server.config import AppConfig, RuntimeConfig
from opensandbox_server.integrations.idle_pause.activity import (
    ActivityRegistry,
    ThreadSafeActivityRecorder,
)
from opensandbox_server.services.fast_sandbox.service import FastSandboxService
from tests.test_idle_pause_activity import FakeAsyncRedis


class RecordingRecorder:
    def __init__(self) -> None:
        self.calls: list[str] = []

    def record(self, sandbox_id: str) -> None:
        self.calls.append(sandbox_id)


class FakeCRReader:
    def get(self, namespace: str, sandbox_id: str) -> dict:
        return {"metadata": {"namespace": "ns1", "uid": "uid-1", "generation": 3}}

    def invalidate(self, namespace: str) -> None: ...


class FakeFastPath:
    def __init__(self) -> None:
        self.resumes: list[dict] = []

    def resume_sandbox(self, namespace: str, sandbox_id: str, **kwargs) -> None:
        self.resumes.append({"namespace": namespace, "sandbox_id": sandbox_id, **kwargs})


def _service() -> tuple[FastSandboxService, FakeFastPath]:
    config = AppConfig(runtime=RuntimeConfig(type="kubernetes", execd_image="x"))
    service = FastSandboxService(config)
    fastpath = FakeFastPath()
    service._cr_reader = FakeCRReader()  # type: ignore[assignment]
    service._fastpath = fastpath  # type: ignore[assignment]
    return service, fastpath


@pytest.mark.asyncio
async def test_resume_records_activity_observation():
    service, fastpath = _service()
    recorder = RecordingRecorder()
    service.set_activity_recorder(recorder)  # type: ignore[arg-type]

    service.resume_sandbox("fsb_1")

    assert fastpath.resumes == [
        {"namespace": "ns1", "sandbox_id": "fsb_1", "expected_uid": "uid-1", "request_id": ""}
    ]
    assert recorder.calls == ["fsb_1"]


@pytest.mark.asyncio
async def test_resume_without_recorder_is_noop():
    service, fastpath = _service()
    service.resume_sandbox("fsb_1")
    assert fastpath.resumes and service._activity_recorder is None


@pytest.mark.asyncio
async def test_threadsafe_recorder_schedules_write_on_running_loop():
    redis = FakeAsyncRedis()
    registry = ActivityRegistry(redis, activity_ttl_seconds=1800)  # type: ignore[arg-type]
    recorder = ThreadSafeActivityRecorder(registry, asyncio.get_running_loop())

    recorder.record("sb_x")
    for _ in range(10):
        await asyncio.sleep(0.01)
        if await registry.read("sb_x") is not None:
            break

    assert await registry.read("sb_x") is not None
