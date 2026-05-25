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
Comprehensive Sync E2E tests for SandboxManagerSync functionality.

Focus: Validate `list_sandbox_infos` filter semantics precisely:
- `states` filter is OR logic
- `metadata` filter is AND logic

We create 3 dedicated sandboxes per run to keep assertions deterministic.
"""

import logging
import time
from datetime import timedelta
from uuid import uuid4

import pytest
from opensandbox import SandboxManagerSync, SandboxSync
from opensandbox.exceptions import SandboxApiException
from opensandbox.models.sandboxes import (
    SandboxFilter,
    SandboxImageSpec,
)

from tests.base_e2e_test import (
    create_connection_config_sync,
    get_sandbox_image,
    is_kubernetes_runtime,
)

logger = logging.getLogger(__name__)

# Kubernetes may use Pending / Allocated during lifecycle; narrow filters omit them and list E2E flakes.
_STATES_OR_BROAD = ["Pending", "Allocated", "Running", "Paused"]
_STATES_NOT_PAUSED = ["Pending", "Allocated", "Running"]


class TestSandboxManagerE2ESync:
    @pytest.mark.timeout(600)
    def test_01_states_filter_or_logic(self):
        cfg = create_connection_config_sync()

        manager = SandboxManagerSync.create(connection_config=cfg)
        tag = f"e2e-sandbox-manager-{uuid4().hex[:8]}"

        s1 = s2 = s3 = None
        try:
            s1 = SandboxSync.create(
                image=SandboxImageSpec(get_sandbox_image()),
                connection_config=cfg,
                resource={"cpu": "100m", "memory": "64Mi"},
                timeout=timedelta(minutes=5),
                ready_timeout=timedelta(seconds=60),
                metadata={"tag": tag, "team": "t1", "env": "prod"},
                env={"E2E_TEST": "true", "EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms", "CASE": "mgr-s1"},
                health_check_polling_interval=timedelta(milliseconds=500),
            )
            s2 = SandboxSync.create(
                image=SandboxImageSpec(get_sandbox_image()),
                connection_config=cfg,
                resource={"cpu": "100m", "memory": "64Mi"},
                timeout=timedelta(minutes=5),
                ready_timeout=timedelta(seconds=60),
                metadata={"tag": tag, "team": "t1", "env": "dev"},
                env={"E2E_TEST": "true", "EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms", "CASE": "mgr-s2"},
                health_check_polling_interval=timedelta(milliseconds=500),
            )
            s3 = SandboxSync.create(
                image=SandboxImageSpec(get_sandbox_image()),
                connection_config=cfg,
                resource={"cpu": "100m", "memory": "64Mi"},
                timeout=timedelta(minutes=5),
                ready_timeout=timedelta(seconds=60),
                metadata={"tag": tag, "env": "prod"},
                env={"E2E_TEST": "true", "EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms", "CASE": "mgr-s3"},
                health_check_polling_interval=timedelta(milliseconds=500),
            )

            assert s1.is_healthy() is True
            assert s2.is_healthy() is True
            assert s3.is_healthy() is True

            s3_paused = False
            if is_kubernetes_runtime():
                logger.warning(
                    "Skipping pause in Kubernetes manager E2E; mini suite does not provision snapshot infra"
                )
            else:
                try:
                    manager.pause_sandbox(s3.id)
                    deadline = time.time() + 180
                    while time.time() < deadline:
                        info = manager.get_sandbox_info(s3.id)
                        if info.status.state == "Paused":
                            break
                        time.sleep(1)
                    assert manager.get_sandbox_info(s3.id).status.state == "Paused"
                    s3_paused = True
                except SandboxApiException as exc:
                    # Some runtimes may not enable pause. Keep all sandboxes Running and relax state-filter asserts.
                    if exc.status_code == 400:
                        logger.warning(
                            "pause_sandbox not configured (HTTP %s); manager state-filter E2E uses all-Running sandboxes",
                            exc.status_code,
                        )
                    else:
                        raise

            # OR states (broad: K8s lifecycle is not only Running/Paused)
            both = manager.list_sandbox_infos(
                SandboxFilter(states=_STATES_OR_BROAD, metadata={"tag": tag}, page_size=50)
            )
            ids = {info.id for info in both.sandbox_infos}
            assert {s1.id, s2.id, s3.id}.issubset(ids)

            paused_only = manager.list_sandbox_infos(
                SandboxFilter(states=["Paused"], metadata={"tag": tag}, page_size=50)
            )
            paused_ids = {info.id for info in paused_only.sandbox_infos}
            running_only = manager.list_sandbox_infos(
                SandboxFilter(states=_STATES_NOT_PAUSED, metadata={"tag": tag}, page_size=50)
            )
            running_ids = {info.id for info in running_only.sandbox_infos}

            if s3_paused:
                assert s3.id in paused_ids
                assert s1.id not in paused_ids
                assert s2.id not in paused_ids
                assert s1.id in running_ids
                assert s2.id in running_ids
                assert s3.id not in running_ids
            else:
                assert s3.id not in paused_ids
                assert s1.id not in paused_ids
                assert s2.id not in paused_ids
                assert s1.id in running_ids
                assert s2.id in running_ids
                assert s3.id in running_ids
        finally:
            for s in [s1, s2, s3]:
                if s is None:
                    continue
                try:
                    s.kill()
                except Exception:
                    pass
                try:
                    s.close()
                except Exception:
                    pass
            manager.close()

    @pytest.mark.timeout(600)
    def test_02_metadata_filter_and_logic(self):
        cfg = create_connection_config_sync()

        manager = SandboxManagerSync.create(connection_config=cfg)
        tag = f"e2e-sandbox-manager-{uuid4().hex[:8]}"

        s1 = s2 = s3 = None
        try:
            s1 = SandboxSync.create(
                image=SandboxImageSpec(get_sandbox_image()),
                connection_config=cfg,
                resource={"cpu": "100m", "memory": "64Mi"},
                timeout=timedelta(minutes=5),
                ready_timeout=timedelta(seconds=60),
                metadata={"tag": tag, "team": "t1", "env": "prod"},
                env={"E2E_TEST": "true", "EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms", "CASE": "mgr-s1"},
                health_check_polling_interval=timedelta(milliseconds=500),
            )
            s2 = SandboxSync.create(
                image=SandboxImageSpec(get_sandbox_image()),
                connection_config=cfg,
                resource={"cpu": "100m", "memory": "64Mi"},
                timeout=timedelta(minutes=5),
                ready_timeout=timedelta(seconds=60),
                metadata={"tag": tag, "team": "t1", "env": "dev"},
                env={"E2E_TEST": "true", "EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms", "CASE": "mgr-s2"},
                health_check_polling_interval=timedelta(milliseconds=500),
            )
            s3 = SandboxSync.create(
                image=SandboxImageSpec(get_sandbox_image()),
                connection_config=cfg,
                resource={"cpu": "100m", "memory": "64Mi"},
                timeout=timedelta(minutes=5),
                ready_timeout=timedelta(seconds=60),
                metadata={"tag": tag, "env": "prod"},
                env={"E2E_TEST": "true", "EXECD_API_GRACE_SHUTDOWN": "3s", "EXECD_JUPYTER_IDLE_POLL_INTERVAL": "200ms", "CASE": "mgr-s3"},
                health_check_polling_interval=timedelta(milliseconds=500),
            )

            assert s1.is_healthy() is True
            assert s2.is_healthy() is True
            assert s3.is_healthy() is True

            # AND metadata
            tag_and_team = manager.list_sandbox_infos(
                SandboxFilter(metadata={"tag": tag, "team": "t1"}, page_size=50)
            )
            ids = {info.id for info in tag_and_team.sandbox_infos}
            assert s1.id in ids
            assert s2.id in ids
            assert s3.id not in ids

            tag_team_env = manager.list_sandbox_infos(
                SandboxFilter(metadata={"tag": tag, "team": "t1", "env": "prod"}, page_size=50)
            )
            ids = {info.id for info in tag_team_env.sandbox_infos}
            assert s1.id in ids
            assert s2.id not in ids
            assert s3.id not in ids

            tag_env = manager.list_sandbox_infos(
                SandboxFilter(metadata={"tag": tag, "env": "prod"}, page_size=50)
            )
            ids = {info.id for info in tag_env.sandbox_infos}
            assert s1.id in ids
            assert s3.id in ids
            assert s2.id not in ids

            none_match = manager.list_sandbox_infos(
                SandboxFilter(metadata={"tag": tag, "team": "t2"}, page_size=50)
            )
            assert all(info.id not in {s1.id, s2.id, s3.id} for info in none_match.sandbox_infos)

            patched = manager.patch_sandbox_metadata(s2.id, {"env": "stage", "team": None})
            assert patched.metadata["env"] == "stage"
            assert "team" not in patched.metadata

            refreshed = manager.get_sandbox_info(s2.id)
            assert refreshed.metadata["env"] == "stage"
            assert "team" not in refreshed.metadata
        finally:
            for s in [s1, s2, s3]:
                if s is None:
                    continue
                try:
                    s.kill()
                except Exception:
                    pass
                try:
                    s.close()
                except Exception:
                    pass
            manager.close()
