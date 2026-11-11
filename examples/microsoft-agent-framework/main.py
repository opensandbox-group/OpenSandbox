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
import asyncio
import os
from datetime import timedelta

from agent_framework import Agent, workflow
from agent_framework.openai import OpenAIChatClient
from dotenv import load_dotenv
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig

load_dotenv()


def _format_execution(execution: object) -> str:
    logs = execution.logs
    stdout = "\n".join(message.text for message in logs.stdout)
    stderr = "\n".join(message.text for message in logs.stderr)

    if execution.error:
        error = execution.error
        stderr = "\n".join(
            part
            for part in (stderr, f"[error] {error.name}: {error.value}")
            if part
        )

    output = stdout.strip()
    if stderr:
        output = "\n".join(part for part in (output, f"[stderr]\n{stderr}") if part)
    return output or "(no output)"


def _create_agent() -> Agent:
    return Agent(
        name="GeneralAssistant",
        client=OpenAIChatClient(),
        instructions=(
            "You are a general-purpose assistant capable of answering questions on "
            "any topic. Answer accurately and concisely in English. Treat runtime "
            "configuration supplied by the application as authoritative."
        ),
    )


async def _close_sandbox(sandbox: Sandbox) -> None:
    try:
        await sandbox.kill()
    finally:
        await sandbox.close()


@workflow
async def sandbox_workflow(question: str) -> str:
    domain = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
    api_key = os.getenv("SANDBOX_API_KEY")
    image = os.getenv(
        "SANDBOX_IMAGE",
        "opensandbox/code-interpreter:v1.1.0",
    )
    commands = [
        os.getenv("SANDBOX_COMMAND", "python3 /tmp/math.py"),
        os.getenv("SANDBOX_FALLBACK_COMMAND", "python /tmp/math.py"),
    ]
    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(seconds=120),
    )

    print("[create] Creating sandbox")
    sandbox = await Sandbox.create(image, connection_config=config)
    print(f"[create] Sandbox ready: {sandbox.id}")

    try:
        print("[prepare] Writing job files")
        await sandbox.files.write_file(
            "/tmp/math.py",
            "result = 137 * 42\nprint(result)\n",
        )
        await sandbox.files.write_file(
            "/tmp/notes.txt",
            "Microsoft Agent Framework + OpenSandbox\n",
        )

        run_output = ""
        last_error = ""
        for attempt, command in enumerate(commands, start=1):
            print(f"[run] Executing job (attempt {attempt}/{len(commands)})")
            execution = await sandbox.commands.run(command)
            run_output = _format_execution(execution)
            print(f"[run] Output: {run_output}")

            if not execution.error:
                last_error = ""
                break

            last_error = f"{execution.error.name}: {execution.error.value}"
            if attempt < len(commands):
                print(f"[run] Failed, retrying with fallback: {commands[attempt]}")

        print("[agent] Asking the general-purpose assistant")
        notes = await sandbox.files.read_file("/tmp/notes.txt")
        configured_model = (
            os.getenv("OPENAI_MODEL", "not specified")
            if os.getenv("OPENAI_API_KEY")
            else os.getenv("AZURE_OPENAI_MODEL", "not specified")
        )
        agent = _create_agent()
        result = await agent.run(
            f"Question: {question}\n\n"
            "Runtime context:\n"
            f"- Configured model: {configured_model}\n"
            "- Sandbox execution status: "
            f"{'failed: ' + last_error if last_error else 'succeeded'}\n"
            f"- Sandbox output: {run_output}\n"
            f"- Sandbox notes: {notes.strip()}"
        )
        return f"Run output:\n{run_output}\n\nAgent answer:\n{result.text}"
    finally:
        print("[cleanup] Cleaning up sandbox")
        await _close_sandbox(sandbox)
        print("[cleanup] Done")


async def main() -> None:
    question = os.getenv("AGENT_QUESTION", "What model are you using?")
    instance = sandbox_workflow.build()
    events = await instance.run(question)
    outputs = events.get_outputs()
    if not outputs:
        raise RuntimeError(
            f"Workflow completed without output: {events.get_final_state()}"
        )
    print(outputs[0])


if __name__ == "__main__":
    asyncio.run(main())
