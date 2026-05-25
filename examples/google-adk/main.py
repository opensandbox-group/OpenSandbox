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

import os
from datetime import timedelta

from google.adk.agents import Agent
from google.adk.apps import App
from google.adk.runners import Runner
from google.adk.sessions.in_memory_session_service import InMemorySessionService
from google.adk.utils._debug_output import print_event
from google.adk.utils.context_utils import Aclosing
from google.genai import types
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig


def _required_env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


async def main() -> None:
    _required_env("GOOGLE_API_KEY")
    domain = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
    api_key = os.getenv("SANDBOX_API_KEY")
    image = os.getenv(
        "SANDBOX_IMAGE",
        "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2",
    )
    model_name = os.getenv("GOOGLE_ADK_MODEL", "gemini-2.5-flash")

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(seconds=120),
    )

    sandbox = await Sandbox.create(
        image,
        connection_config=config,
    )

    async def run_in_sandbox(command: str) -> str:
        """Run a shell command in OpenSandbox and return the output."""

        execution = await sandbox.commands.run(command)
        stdout = "\n".join(msg.text for msg in execution.logs.stdout)
        stderr = "\n".join(msg.text for msg in execution.logs.stderr)
        if execution.error:
            stderr = "\n".join(
                [
                    stderr,
                    f"[error] {execution.error.name}: {execution.error.value}",
                ]
            ).strip()

        output = stdout.strip()
        if stderr:
            output = "\n".join([output, f"[stderr]\n{stderr}"]).strip()
        return output or "(no output)"

    async def write_file(path: str, content: str) -> str:
        """Write a file inside the sandbox."""

        await sandbox.files.write_file(path, content)
        return f"wrote {len(content)} bytes to {path}"

    async def read_file(path: str) -> str:
        """Read a file from the sandbox."""

        return await sandbox.files.read_file(path)

    agent = Agent(
        name="opensandbox_adk",
        model=model_name,
        instruction=(
            "You have access to OpenSandbox tools. Use write_file to create or "
            "update files, read_file to read files, and run_in_sandbox to run "
            "commands."
        ),
        tools=[run_in_sandbox, write_file, read_file],
    )

    app = App(name="opensandbox_adk", root_agent=agent)
    session_service = InMemorySessionService()
    runner = Runner(app=app, session_service=session_service)
    session = await session_service.create_session(
        app_name=app.name,
        user_id="local-user",
    )

    prompts = [
        "Use write_file to save /tmp/math.py that prints 137 * 42.",
        "Run the script using run_in_sandbox and report the result.",
        "Write /tmp/notes.txt with 'ADK + OpenSandbox', then read it back.",
    ]

    try:
        for prompt in prompts:
            content = types.Content(
                role="user",
                parts=[types.Part(text=prompt)],
            )
            async with Aclosing(
                runner.run_async(
                    user_id=session.user_id,
                    session_id=session.id,
                    new_message=content,
                )
            ) as agen:
                async for event in agen:
                    print_event(event, verbose=True)
    finally:
        await sandbox.kill()
        await sandbox.close()


if __name__ == "__main__":
    import asyncio

    asyncio.run(main())
