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
from datetime import timedelta

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig


def _required_env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


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
    claude_auth_token = _required_env("ANTHROPIC_AUTH_TOKEN")
    claude_base_url = os.getenv("ANTHROPIC_BASE_URL")
    claude_model_name = os.getenv("ANTHROPIC_MODEL", "claude_sonnet4")
    image = os.getenv(
        "SANDBOX_IMAGE",
        "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2",
    )

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(seconds=60),
    )

    # Inject Claude settings into container environment for CLI access
    env = {
        "ANTHROPIC_AUTH_TOKEN": claude_auth_token,
        "ANTHROPIC_BASE_URL": claude_base_url,
        "ANTHROPIC_MODEL": claude_model_name,
        "IS_SANDBOX": "1",
    }
    # Drop None values to avoid overriding defaults inside CLI
    env = {k: v for k, v in env.items() if v is not None}

    sandbox = await Sandbox.create(
        image,
        connection_config=config,
        env=env,
    )

    async with sandbox:
        # Install Claude CLI (Node.js is already in the code-interpreter image)
        install_exec = await sandbox.commands.run(
            "npm i -g @anthropic-ai/claude-code@latest"
        )
        await _print_execution_logs(install_exec)

        # Use Claude CLI to send a message
        run_exec = await sandbox.commands.run(
            'claude "Compute 1+1=?."'
        )
        await _print_execution_logs(run_exec)

        await sandbox.kill()


if __name__ == "__main__":
    asyncio.run(main())
