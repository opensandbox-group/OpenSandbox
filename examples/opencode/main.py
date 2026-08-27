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

import asyncio
import os
import shlex
from datetime import timedelta

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig


async def _print_execution_logs(execution) -> None:
    for msg in execution.logs.stdout:
        print(f"[stdout] {msg.text}")
    for msg in execution.logs.stderr:
        print(f"[stderr] {msg.text}")
    if execution.error:
        print(f"[error] {execution.error.name}: {execution.error.value}")


async def main() -> None:
    domain = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
    api_key = os.getenv("SANDBOX_API_KEY")
    opencode_api_key = os.getenv("OPENCODE_API_KEY")
    opencode_model = os.getenv("OPENCODE_MODEL", "opencode/deepseek-v4-flash-free")
    image = os.getenv(
        "SANDBOX_IMAGE",
        "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0",
    )

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(seconds=60),
    )

    env = {"OPENCODE_API_KEY": opencode_api_key}
    env = {key: value for key, value in env.items() if value is not None}

    sandbox = await Sandbox.create(
        image,
        connection_config=config,
        env=env,
    )

    try:
        # Install OpenCode (Node.js is already in the code-interpreter image).
        install_exec = await sandbox.commands.run("npm install -g opencode-ai@latest")
        await _print_execution_logs(install_exec)

        # Run OpenCode non-interactively in an isolated working directory.
        run_exec = await sandbox.commands.run(
            "mkdir -p /tmp/opencode-example && "
            "cd /tmp/opencode-example && "
            f"opencode run --model {shlex.quote(opencode_model)} "
            '"Compute 1+1 and reply with only the final number."'
        )
        await _print_execution_logs(run_exec)
    finally:
        await sandbox.destroy()


if __name__ == "__main__":
    asyncio.run(main())
