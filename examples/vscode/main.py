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
from datetime import timedelta

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models.execd import RunCommandOpts


def _required_env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


async def _print_logs(label: str, execution) -> None:
    for msg in execution.logs.stdout:
        print(f"[{label} stdout] {msg.text}")
    for msg in execution.logs.stderr:
        print(f"[{label} stderr] {msg.text}")
    if execution.error:
        print(f"[{label} error] {execution.error.name}: {execution.error.value}")


async def main() -> None:
    domain = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
    api_key = os.getenv("SANDBOX_API_KEY")
    image = os.getenv(
        "SANDBOX_IMAGE",
        "opensandbox/vscode:latest",
    )
    python_version = os.getenv("PYTHON_VERSION", "3.11")
    code_port = int(os.getenv("CODE_PORT", "8443"))

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(seconds=60),
    )

    # Inject Python version into container environment
    env = {"PYTHON_VERSION": python_version}
    sandbox = await Sandbox.create(
        image,
        connection_config=config,
        env=env,
    )

    async with sandbox:
        # code-server is pre-installed in the image
        # Start code-server with authentication disabled
        start_exec = await sandbox.commands.run(
            f"code-server --bind-addr 0.0.0.0:{code_port} --auth none /workspace",
            opts=RunCommandOpts(background=True),
        )
        await _print_logs("code-server", start_exec)

        endpoint = await sandbox.get_endpoint(code_port)
        print("\nVS Code Web endpoint:")
        print(f"  http://{endpoint.endpoint}/")

        print("\nKeeping sandbox alive for 10 minutes. Press Ctrl+C to exit sooner.")
        try:
            await asyncio.sleep(600)
        except KeyboardInterrupt:
            print("Stopping...")
        finally:
            await sandbox.kill()


if __name__ == "__main__":
    asyncio.run(main())
