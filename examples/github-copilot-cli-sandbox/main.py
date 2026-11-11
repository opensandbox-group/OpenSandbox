# Copyright 2025 The OpenSandbox Authors
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

from dotenv import load_dotenv
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig


def _required_token() -> str:
    token = os.getenv("GH_TOKEN") or os.getenv("GITHUB_TOKEN")
    if token:
        return token
    raise RuntimeError(
        "GH_TOKEN or GITHUB_TOKEN is required and must have "
        "the 'Copilot Requests' permission"
    )


async def _print_execution_logs(execution) -> None:
    for message in execution.logs.stdout:
        print(f"[stdout] {message.text}")
    for message in execution.logs.stderr:
        print(f"[stderr] {message.text}")
    if execution.error:
        raise RuntimeError(
            f"{execution.error.name}: {execution.error.value}"
        )


async def main() -> None:
    load_dotenv()

    domain = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
    api_key = os.getenv("SANDBOX_API_KEY")
    image = os.getenv(
        "SANDBOX_IMAGE",
        "opensandbox/code-interpreter:v1.1.0",
    )
    prompt = os.getenv(
        "COPILOT_PROMPT",
        "Compute 1+1 and reply with only the final number.",
    )
    version = os.getenv("COPILOT_VERSION")

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(seconds=60),
    )
    sandbox = await Sandbox.create(
        image,
        connection_config=config,
        env={"GH_TOKEN": _required_token()},
    )

    try:
        version_env = (
            f"VERSION={shlex.quote(version)} " if version else ""
        )
        install = await sandbox.commands.run(
            "curl -fsSL https://gh.io/copilot-install "
            "-o /tmp/install-copilot.sh && "
            f"{version_env}PREFIX=/tmp/copilot "
            'PATH="/tmp/copilot/bin:$PATH" '
            "bash /tmp/install-copilot.sh && "
            "rm /tmp/install-copilot.sh"
        )
        await _print_execution_logs(install)

        run = await sandbox.commands.run(
            "mkdir -p /tmp/copilot-workspace && "
            "cd /tmp/copilot-workspace && "
            f"/tmp/copilot/bin/copilot -sp {shlex.quote(prompt)}"
        )
        await _print_execution_logs(run)
    finally:
        await sandbox.destroy()


if __name__ == "__main__":
    asyncio.run(main())
