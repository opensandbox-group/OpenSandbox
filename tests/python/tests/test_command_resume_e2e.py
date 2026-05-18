#
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
#
"""
E2E tests for SSE stream disconnect and automatic resume.

Creates a dedicated sandbox, injects a mid-stream disconnect via a custom
httpx transport, and verifies the SDK transparently resumes via the real
execd resume endpoint.
"""

from __future__ import annotations

import logging
from datetime import timedelta

import httpx
import pytest
from opensandbox.adapters.command_adapter import CommandsAdapter
from opensandbox.config import ConnectionConfig
from opensandbox.constants import DEFAULT_EXECD_PORT
from opensandbox.models.sandboxes import Host, SandboxEndpoint, SandboxImageSpec, Volume
from opensandbox.sandbox import Sandbox

from tests.base_e2e_test import (
    TEST_API_KEY,
    TEST_DOMAIN,
    TEST_PROTOCOL,
    create_connection_config,
    get_e2e_sandbox_resource,
    get_sandbox_image,
    should_use_server_proxy,
)

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Custom async transport: injects disconnect mid-stream on POST /command
# ---------------------------------------------------------------------------


class _DisconnectInjectStream(httpx.AsyncByteStream):
    """Wraps real byte stream; raises ReadError after N chunks yielded."""

    def __init__(self, real_stream, disconnect_after_chunks: int = 4) -> None:
        self._real = real_stream
        self._disconnect_after = disconnect_after_chunks
        self._chunk_count = 0
        self._real_iter = None

    def __aiter__(self):
        self._real_iter = self._real.__aiter__()
        return self

    async def __anext__(self) -> bytes:
        if self._chunk_count >= self._disconnect_after:
            raise httpx.ReadError("simulated disconnect for e2e test")
        try:
            chunk = await self._real_iter.__anext__()
        except StopAsyncIteration:
            raise
        self._chunk_count += 1
        return chunk

    async def aclose(self) -> None:
        if self._real_iter is not None and hasattr(self._real_iter, "aclose"):
            await self._real_iter.aclose()
        elif hasattr(self._real, "aclose"):
            await self._real.aclose()


class _DisconnectInjectTransport(httpx.AsyncHTTPTransport):
    """Wraps real transport; injects stream disconnect on POST command endpoints."""

    def __init__(self) -> None:
        self._real = httpx.AsyncHTTPTransport()
        self.post_count: int = 0
        self.resume_count: int = 0
        self.last_resume_eid: int | None = None

    @staticmethod
    def _is_command_post(path: str) -> bool:
        """Match POST /command or POST /session/:id/run (with optional proxy prefix)."""
        return path == "/command" or path.endswith("/command") or path.endswith("/run")

    async def handle_async_request(self, request: httpx.Request) -> httpx.Response:
        response = await self._real.handle_async_request(request)

        if request.method == "POST" and self._is_command_post(request.url.path):
            self.post_count += 1
            response.stream = _DisconnectInjectStream(
                response.stream, disconnect_after_chunks=4
            )

        elif request.method == "GET" and "/resume" in request.url.path:
            self.resume_count += 1
            qp = request.url.params.get("after_eid")
            if qp:
                self.last_resume_eid = int(qp)

        return response


# ---------------------------------------------------------------------------
# Shared sandbox fixture — created once per class, killed on teardown
# ---------------------------------------------------------------------------


@pytest.mark.asyncio
class TestCommandResumeE2E:
    """E2E tests: SSE disconnect mid-stream triggers transparent resume."""

    sandbox: Sandbox | None = None
    execd_endpoint: SandboxEndpoint | None = None

    @pytest.fixture(scope="class", autouse=True)
    async def _sandbox_lifecycle(self, request: pytest.FixtureRequest) -> None:
        """Create a dedicated sandbox and ALWAYS clean it up."""
        logger.info("=" * 80)
        logger.info("SETUP: Creating sandbox for SSE resume E2E")
        logger.info("=" * 80)

        connection_config = create_connection_config()

        sandbox = await Sandbox.create(
            image=SandboxImageSpec(get_sandbox_image()),
            entrypoint=["/opt/opensandbox/code-interpreter.sh"],
            connection_config=connection_config,
            resource=get_e2e_sandbox_resource(),
            timeout=timedelta(minutes=15),
            ready_timeout=timedelta(seconds=60),
            metadata={"tag": "e2e-command-resume"},
            env={
                "E2E_TEST": "true",
                "EXECD_LOG_FILE": "/tmp/opensandbox-e2e/logs/execd-resume.log",
                "EXECD_API_GRACE_SHUTDOWN": "3s",
            },
            health_check_polling_interval=timedelta(milliseconds=500),
            volumes=[
                Volume(
                    name="execd-log",
                    host=Host(path="/tmp/opensandbox-e2e/logs"),
                    mountPath="/tmp/opensandbox-e2e/logs",
                    readOnly=False,
                ),
            ],
        )

        endpoint_obj = await sandbox.get_endpoint(DEFAULT_EXECD_PORT)
        assert endpoint_obj is not None
        assert endpoint_obj.endpoint

        request.cls.sandbox = sandbox
        request.cls.execd_endpoint = endpoint_obj

        logger.info("Sandbox ready: %s  execd endpoint: %s", sandbox.id, endpoint_obj.endpoint)

        try:
            yield
        finally:
            if sandbox is not None:
                try:
                    await sandbox.kill()
                except Exception as e:
                    logger.warning("Teardown: sandbox.kill() failed: %s", e, exc_info=True)
                try:
                    await sandbox.close()
                except Exception as e:
                    logger.warning("Teardown: sandbox.close() failed: %s", e, exc_info=True)

    # -----------------------------------------------------------------------
    # Test: standalone command with injected disconnect
    # -----------------------------------------------------------------------

    @pytest.mark.timeout(120)
    async def test_run_command_auto_resume_on_disconnect(self) -> None:
        """Inject disconnect mid-stream; verify resume fires and all events arrive."""
        transport = _DisconnectInjectTransport()
        cfg = ConnectionConfig(
            domain=TEST_DOMAIN,
            api_key=TEST_API_KEY,
            transport=transport,
            protocol=TEST_PROTOCOL,
            request_timeout=timedelta(minutes=3),
            use_server_proxy=should_use_server_proxy(),
        )

        adapter = CommandsAdapter(cfg, self.execd_endpoint)

        # Long-ish command with sleeps so events flush in separate chunks
        cmd = (
            "for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20; "
            "do echo \"line$i\"; sleep 0.2; done"
        )

        execution = await adapter.run(cmd)

        # --- assertions: resume actually happened ---
        assert transport.post_count == 1, "should send exactly one POST /command"
        assert transport.resume_count >= 1, (
            f"should resume at least once, got resume_count={transport.resume_count}"
        )
        assert transport.last_resume_eid is not None, "resume should include after_eid"
        assert transport.last_resume_eid >= 1, (
            f"after_eid should be >=1, got {transport.last_resume_eid}"
        )

        # --- assertions: all output received ---
        assert len(execution.logs.stdout) == 20, (
            f"expected 20 stdout lines, got {len(execution.logs.stdout)}"
        )
        for i, msg in enumerate(execution.logs.stdout):
            expected = f"line{i + 1}"
            actual = msg.text.strip()
            assert actual == expected, f"stdout[{i}]: expected {expected!r}, got {actual!r}"

        assert execution.complete is not None, "should have completion event"
        assert execution.complete.execution_time_in_millis >= 0
        assert execution.exit_code == 0

        logger.info(
            "Resume e2e: post=%d resume=%d after_eid=%d lines=%d",
            transport.post_count,
            transport.resume_count,
            transport.last_resume_eid,
            len(execution.logs.stdout),
        )

    # NOTE: run_in_session resume is intentionally NOT tested here.
    # execd does not currently support resume for session-scoped commands:
    #   - No resume route under /session group
    #   - GetCommandStatus only looks in commandClientMap (sessions use bashSessionClientMap)
    #   - resumeEnabled is never set for RunInSession
    # The SDK will attempt resume on disconnect but execd returns 404.
