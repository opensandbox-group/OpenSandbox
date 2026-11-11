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
import json
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


def _jsonl_events(execution):
    """Yield parsed events from the JSON Lines stream of `codex exec --json`."""
    text = "\n".join(msg.text for msg in execution.logs.stdout)
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            yield json.loads(line)
        except json.JSONDecodeError:
            print(f"[headless] skipped non-JSON line: {line[:80]}")


async def main() -> None:
    domain = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
    api_key = os.getenv("SANDBOX_API_KEY")
    openai_api_key = _required_env("OPENAI_API_KEY")
    openai_base_url = os.getenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
    openai_model = os.getenv("OPENAI_MODEL", "gpt-4o-mini")
    image = os.getenv(
        "SANDBOX_IMAGE",
        "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0",
    )

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(seconds=60),
    )

    # Inject OpenAI settings into container environment for CLI access
    env = {
        "OPENAI_API_KEY": openai_api_key,
        "OPENAI_BASE_URL": openai_base_url,
        "OPENAI_MODEL": openai_model,
    }
    # Drop None values to avoid overriding defaults inside CLI
    env = {k: v for k, v in env.items() if v is not None}

    sandbox = await Sandbox.create(
        image,
        connection_config=config,
        env=env,
    )

    async with sandbox:
        # Install Codex CLI (Node.js is already in the code-interpreter image)
        install_exec = await sandbox.commands.run(
            "npm install -g @openai/codex@latest"
        )
        await _print_execution_logs(install_exec)

        # Use Codex CLI to execute a command
        run_exec = await sandbox.commands.run(
            'codex exec "Compute 1+1=?." --skip-git-repo-check'
        )
        await _print_execution_logs(run_exec)

        # Headless run with structured output: --json turns stdout into a
        # JSON Lines (JSONL) event stream. The first event, thread.started,
        # carries the thread id, which is the session id that
        # `codex exec resume` accepts.
        headless_exec = await sandbox.commands.run(
            'codex exec --json "Remember this for later: my favorite sandbox number is 42." '
            "--skip-git-repo-check"
        )
        thread_id = ""
        for event in _jsonl_events(headless_exec):
            if event.get("type") == "thread.started":
                thread_id = event.get("thread_id", "")
                print(f"[headless] thread_id: {thread_id}")

        # Resume the same session for a follow-up turn: the model recalls the
        # context of the previous turns, so the reply is "42".
        if thread_id:
            resume_exec = await sandbox.commands.run(
                f'codex exec resume "{thread_id}" '
                '"What is my favorite sandbox number? Reply with just the number." '
                "--skip-git-repo-check"
            )
            await _print_execution_logs(resume_exec)

        await sandbox.kill()


if __name__ == "__main__":
    asyncio.run(main())
