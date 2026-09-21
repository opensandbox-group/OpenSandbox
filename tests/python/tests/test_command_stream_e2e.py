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

"""Exercise foreground command stream completion over real sandbox transports."""

import asyncio
import json
import logging
import shlex
import time
from datetime import timedelta

import pytest
from opensandbox import Sandbox
from opensandbox.models.execd import ExecutionHandlers
from opensandbox.models.sandboxes import NetworkPolicy

from tests.base_e2e_test import (
    create_connection_config,
    create_connection_config_server_proxy,
    get_e2e_sandbox_resource,
    get_sandbox_image,
    is_kubernetes_runtime,
)

logger = logging.getLogger(__name__)
pytestmark = pytest.mark.skipif(
    is_kubernetes_runtime(),
    reason="This matrix requires Docker bridge endpoints reachable from the test runner.",
)


@pytest.fixture(
    scope="module",
    params=[(False, False), (True, False), (False, True), (True, True)],
    ids=["direct", "server-proxy", "direct-egress", "server-proxy-egress"],
)
async def stream_sandbox(request):
    proxy, egress = request.param
    config = (
        create_connection_config_server_proxy() if proxy else create_connection_config()
    )
    # Exercise both paths even when the surrounding E2E job defaults to proxying.
    config.use_server_proxy = proxy
    sandbox = await Sandbox.create(
        image=get_sandbox_image(),
        connection_config=config,
        resource=get_e2e_sandbox_resource(),
        timeout=timedelta(minutes=5),
        env={"EXECD_API_GRACE_SHUTDOWN": "1s"},
        network_policy=NetworkPolicy(defaultAction="allow") if egress else None,
        metadata={"tag": "command-stream-e2e"},
    )
    try:
        yield sandbox, proxy, egress
    finally:
        try:
            await sandbox.kill()
        finally:
            await sandbox.close()


@pytest.mark.parametrize("scenario", ["empty", "burst", "burst-error"])
async def test_foreground_command_stream_completion(stream_sandbox, scenario):
    sandbox, proxy, egress = stream_sandbox
    exit_code = 7 if scenario == "burst-error" else 0
    expected_stdout = []
    expected_stderr = []
    command = "true"
    if scenario != "empty":
        # Exceed small HTTP/pipe buffers, keep every line distinguishable, and
        # finish with unterminated lines to exercise the final output drain.
        expected_stdout = [f"out-{i:04d}-" + "x" * 4096 for i in range(512)]
        expected_stderr = [f"err-{i:04d}-" + "y" * 4096 for i in range(512)]
        expected_stdout.append("stdout-tail-你好")
        expected_stderr.append("stderr-tail-世界")
        script = (
            "import sys\n"
            "for i in range(512):\n"
            "    sys.stdout.write(f'out-{i:04d}-' + 'x' * 4096 + '\\n')\n"
            "    sys.stderr.write(f'err-{i:04d}-' + 'y' * 4096 + '\\n')\n"
            "sys.stdout.write('stdout-tail-你好')\n"
            "sys.stderr.write('stderr-tail-世界')\n"
            f"sys.exit({exit_code})\n"
        )
        command = "python3 -c " + shlex.quote(script)

    stdout = []
    stderr = []
    events = []
    terminal_at = None

    async def on_init(_):
        events.append("init")
        if scenario != "empty":
            # Leave a real producer running while the SDK consumer is delayed.
            await asyncio.sleep(0.1)

    async def on_stdout(message):
        stdout.append(message.text)
        events.append("stdout")

    async def on_stderr(message):
        stderr.append(message.text)
        events.append("stderr")

    async def on_complete(_):
        nonlocal terminal_at
        terminal_at = time.perf_counter()
        events.append("complete")

    async def on_error(_):
        nonlocal terminal_at
        terminal_at = time.perf_counter()
        events.append("error")

    started = time.perf_counter()
    execution = await sandbox.commands.run(
        command,
        handlers=ExecutionHandlers(
            on_init=on_init,
            on_stdout=on_stdout,
            on_stderr=on_stderr,
            on_execution_complete=on_complete,
            on_error=on_error,
        ),
    )
    returned = time.perf_counter()

    assert execution.exit_code == exit_code
    assert (execution.error is None) == (exit_code == 0)
    assert stdout == expected_stdout
    assert stderr == expected_stderr
    assert [message.text for message in execution.logs.stdout] == expected_stdout
    assert [message.text for message in execution.logs.stderr] == expected_stderr
    assert events[0] == "init"
    assert events.count("init") == 1
    terminal = "error" if exit_code else "complete"
    assert events[-1] == terminal
    assert events.count("complete") + events.count("error") == 1
    assert terminal_at is not None

    # Keep timing observable for A/B validation without imposing a machine-speed
    # threshold on normal E2E runs (which may use an older published execd).
    logger.info(
        "COMMAND_STREAM_METRIC %s",
        json.dumps(
            {
                "proxy": proxy,
                "egress": egress,
                "scenario": scenario,
                "total_ms": round((returned - started) * 1000, 1),
                "terminal_to_return_ms": round((returned - terminal_at) * 1000, 1),
                "stdout_lines": len(stdout),
                "stderr_lines": len(stderr),
            }
        ),
    )
