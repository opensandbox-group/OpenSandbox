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
"""
Comprehensive E2E tests for SandboxManager functionality.

Focus: Validate `list_sandbox_infos` filter semantics precisely:
- `states` filter is OR logic
- `metadata` filter is AND logic

We create 3 dedicated sandboxes per run to keep assertions deterministic.
"""

import asyncio
import logging
import time
from datetime import timedelta
from uuid import uuid4

import pytest
from opensandbox import Sandbox, SandboxManager
from opensandbox.config import ConnectionConfig
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.sandboxes import (
    SandboxFilter,
    SandboxImageSpec,
)

from tests.base_e2e_test import (
    create_connection_config,
    get_sandbox_image,
    is_kubernetes_runtime,
)

logger = logging.getLogger(__name__)

# Kubernetes may use Pending / Allocated during lifecycle; narrow filters omit them and list E2E flakes.
_STATES_OR_BROAD = ["Pending", "Allocated", "Running", "Paused"]
_STATES_NOT_PAUSED = ["Pending", "Allocated", "Running"]


async def _create_sandbox(
    *,
    connection_config: ConnectionConfig,
    image: str,
    metadata: dict[str, str],
    env: dict[str, str],
    timeout: timedelta,
    ready_timeout: timedelta,
) -> Sandbox:
    return await Sandbox.create(
        image=SandboxImageSpec(image),
        connection_config=connection_config,
        resource={"cpu": "100m", "memory": "64Mi"},
        timeout=timeout,
        ready_timeout=ready_timeout,
        metadata=metadata,
        env=env,
        health_check_polling_interval=timedelta(milliseconds=500),
    )


async def _wait_for_state(
    *,
    manager: SandboxManager,
    sandbox_id,
    expected_state: str,
    timeout: timedelta = timedelta(minutes=3),
) -> None:
    deadline = time.time() + timeout.total_seconds()
    last_state = None
    while time.time() < deadline:
        info = await manager.get_sandbox_info(sandbox_id)
        last_state = info.status.state
        if last_state == expected_state:
            return
        await asyncio.sleep(1)
    raise AssertionError(f"Timed out waiting for state={expected_state}, last_state={last_state}")


@pytest.mark.asyncio
class TestSandboxManagerE2E:
    """E2E tests for SandboxManager list/filter semantics."""

    connection_config: ConnectionConfig | None = None
    manager: SandboxManager | None = None
    tag: str | None = None
    s1: Sandbox | None = None
    s2: Sandbox | None = None
    s3: Sandbox | None = None
    #: True if s3 was paused successfully; False when pause is unsupported or intentionally skipped.
    s3_paused: bool = False

    @pytest.fixture(scope="class", autouse=True)
    async def _manager_setup(self, request):
        cls = request.cls
        # Create connection config (user-owned transport; we close it explicitly).
        cls.connection_config = create_connection_config()

        cls.manager = await SandboxManager.create(connection_config=cls.connection_config)
        cls.tag = f"e2e-sandbox-manager-{uuid4().hex[:8]}"

        # Create 3 sandboxes with controlled metadata.
        # s1: tag + team=t1 + env=prod
        # s2: tag + team=t1 + env=dev
        # s3: tag + env=prod (no team). Docker pauses it to cover Paused filters;
        # Kubernetes mini keeps it active because the suite does not provision snapshot infra.
        cls.s1 = await _create_sandbox(
            connection_config=cls.connection_config,
            image=get_sandbox_image(),
            metadata={"tag": cls.tag, "team": "t1", "env": "prod"},
            env={"E2E_TEST": "true", "CASE": "mgr-s1", "EXECD_API_GRACE_SHUTDOWN": "3s",
                "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=60),
        )
        cls.s2 = await _create_sandbox(
            connection_config=cls.connection_config,
            image=get_sandbox_image(),
            metadata={"tag": cls.tag, "team": "t1", "env": "dev"},
            env={"E2E_TEST": "true", "CASE": "mgr-s2", "EXECD_API_GRACE_SHUTDOWN": "3s",
                "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=60),
        )
        cls.s3 = await _create_sandbox(
            connection_config=cls.connection_config,
            image=get_sandbox_image(),
            metadata={"tag": cls.tag, "env": "prod"},
            env={"E2E_TEST": "true", "CASE": "mgr-s3", "EXECD_API_GRACE_SHUTDOWN": "3s",
                "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms"},
            timeout=timedelta(minutes=5),
            ready_timeout=timedelta(seconds=60),
        )

        assert await cls.s1.is_healthy() is True
        assert await cls.s2.is_healthy() is True
        assert await cls.s3.is_healthy() is True

        cls.s3_paused = False
        if is_kubernetes_runtime():
            logger.warning(
                "Skipping pause in Kubernetes manager E2E; mini suite does not provision snapshot infra"
            )
        else:
            try:
                await cls.manager.pause_sandbox(cls.s3.id)
                await _wait_for_state(
                    manager=cls.manager, sandbox_id=cls.s3.id, expected_state="Paused"
                )
                cls.s3_paused = True
            except SandboxApiException as exc:
                # Some runtimes may not enable pause. Keep all sandboxes Running and relax state-filter asserts.
                if exc.status_code == 400:
                    logger.warning(
                        "pause_sandbox not configured (HTTP %s); manager state-filter E2E uses all-Running sandboxes",
                        exc.status_code,
                    )
                else:
                    raise

        try:
            yield
        finally:
            # Best-effort cleanup: kill sandboxes (remote) and close local resources.
            for s in [cls.s1, cls.s2, cls.s3]:
                if s is None:
                    continue
                try:
                    await s.kill()
                except Exception:
                    pass
                try:
                    await s.close()
                except Exception:
                    pass

            if cls.manager is not None:
                try:
                    await cls.manager.close()
                except Exception:
                    pass

            if cls.connection_config is not None:
                try:
                    await cls.connection_config.transport.aclose()
                except Exception:
                    pass

    @pytest.mark.timeout(600)
    async def test_01_states_filter_or_logic(self):
        manager = TestSandboxManagerE2E.manager
        assert manager is not None
        assert TestSandboxManagerE2E.tag is not None
        assert TestSandboxManagerE2E.s1 is not None and TestSandboxManagerE2E.s2 is not None and TestSandboxManagerE2E.s3 is not None

        # states filter is OR: should return sandboxes in ANY of the requested states.
        result = await manager.list_sandbox_infos(
            SandboxFilter(
                states=_STATES_OR_BROAD,
                metadata={"tag": TestSandboxManagerE2E.tag},
                page_size=50,
            )
        )
        ids = {info.id for info in result.sandbox_infos}
        assert {TestSandboxManagerE2E.s1.id, TestSandboxManagerE2E.s2.id, TestSandboxManagerE2E.s3.id}.issubset(ids)

        paused_only = await manager.list_sandbox_infos(
            SandboxFilter(states=["Paused"], metadata={"tag": TestSandboxManagerE2E.tag}, page_size=50)
        )
        paused_ids = {info.id for info in paused_only.sandbox_infos}
        running_only = await manager.list_sandbox_infos(
            SandboxFilter(
                states=_STATES_NOT_PAUSED,
                metadata={"tag": TestSandboxManagerE2E.tag},
                page_size=50,
            )
        )
        running_ids = {info.id for info in running_only.sandbox_infos}

        if TestSandboxManagerE2E.s3_paused:
            assert TestSandboxManagerE2E.s3.id in paused_ids
            assert TestSandboxManagerE2E.s1.id not in paused_ids
            assert TestSandboxManagerE2E.s2.id not in paused_ids
            assert TestSandboxManagerE2E.s1.id in running_ids
            assert TestSandboxManagerE2E.s2.id in running_ids
            assert TestSandboxManagerE2E.s3.id not in running_ids
        else:
            assert TestSandboxManagerE2E.s3.id not in paused_ids
            assert TestSandboxManagerE2E.s1.id not in paused_ids
            assert TestSandboxManagerE2E.s2.id not in paused_ids
            assert TestSandboxManagerE2E.s1.id in running_ids
            assert TestSandboxManagerE2E.s2.id in running_ids
            assert TestSandboxManagerE2E.s3.id in running_ids

    @pytest.mark.timeout(600)
    async def test_02_metadata_filter_and_logic(self):
        manager = TestSandboxManagerE2E.manager
        assert manager is not None
        assert TestSandboxManagerE2E.tag is not None
        assert TestSandboxManagerE2E.s1 is not None and TestSandboxManagerE2E.s2 is not None and TestSandboxManagerE2E.s3 is not None

        # metadata filter is AND across all key-value pairs.
        # tag+team=t1 should match s1 and s2 (both have team=t1), not s3.
        tag_and_team = await manager.list_sandbox_infos(
            SandboxFilter(metadata={"tag": TestSandboxManagerE2E.tag, "team": "t1"}, page_size=50)
        )
        ids = {info.id for info in tag_and_team.sandbox_infos}
        assert TestSandboxManagerE2E.s1.id in ids
        assert TestSandboxManagerE2E.s2.id in ids
        assert TestSandboxManagerE2E.s3.id not in ids

        # tag+team=t1+env=prod should match only s1 (AND narrows results).
        tag_team_env = await manager.list_sandbox_infos(
            SandboxFilter(metadata={"tag": TestSandboxManagerE2E.tag, "team": "t1", "env": "prod"}, page_size=50)
        )
        ids = {info.id for info in tag_team_env.sandbox_infos}
        assert TestSandboxManagerE2E.s1.id in ids
        assert TestSandboxManagerE2E.s2.id not in ids
        assert TestSandboxManagerE2E.s3.id not in ids

        # tag+env=prod should match s1 and s3.
        tag_env = await manager.list_sandbox_infos(
            SandboxFilter(metadata={"tag": TestSandboxManagerE2E.tag, "env": "prod"}, page_size=50)
        )
        ids = {info.id for info in tag_env.sandbox_infos}
        assert TestSandboxManagerE2E.s1.id in ids
        assert TestSandboxManagerE2E.s3.id in ids
        assert TestSandboxManagerE2E.s2.id not in ids

        # Negative: tag+team=t2 should match none.
        none_match = await manager.list_sandbox_infos(
            SandboxFilter(metadata={"tag": TestSandboxManagerE2E.tag, "team": "t2"}, page_size=50)
        )
        assert all(
            info.id not in {TestSandboxManagerE2E.s1.id, TestSandboxManagerE2E.s2.id, TestSandboxManagerE2E.s3.id}
            for info in none_match.sandbox_infos
        )

        patched = await manager.patch_sandbox_metadata(
            TestSandboxManagerE2E.s2.id,
            {"env": "stage", "team": None},
        )
        assert patched.metadata["env"] == "stage"
        assert "team" not in patched.metadata

        refreshed = await manager.get_sandbox_info(TestSandboxManagerE2E.s2.id)
        assert refreshed.metadata["env"] == "stage"
        assert "team" not in refreshed.metadata
