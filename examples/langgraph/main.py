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
from typing import TypedDict

from langchain_anthropic import ChatAnthropic
from langgraph.graph import END, StateGraph
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig


class WorkflowState(TypedDict):
    sandbox: Sandbox | None
    run_output: str
    summary: str
    last_error: str
    attempt: int
    max_attempts: int
    command: str
    fallback_command: str
    cleaned: bool


def _configure_anthropic_env() -> None:
    api_key = os.getenv("ANTHROPIC_API_KEY")
    auth_token = os.getenv("ANTHROPIC_AUTH_TOKEN")

    if auth_token:
        os.environ["ANTHROPIC_AUTH_TOKEN"] = auth_token
        os.environ.pop("ANTHROPIC_API_KEY", None)
        return

    if api_key:
        os.environ["ANTHROPIC_API_KEY"] = api_key
        os.environ.pop("ANTHROPIC_AUTH_TOKEN", None)
        return

    raise RuntimeError("ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is required")


def _build_llm() -> ChatAnthropic:
    _configure_anthropic_env()
    anthropic_base_url = os.getenv("ANTHROPIC_BASE_URL")
    model_name = os.getenv("ANTHROPIC_MODEL", "claude-3-5-sonnet-latest")

    return ChatAnthropic(
        model=model_name,
        anthropic_api_url=anthropic_base_url,
    )


def _format_execution(execution) -> str:
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


async def create_sandbox(state: WorkflowState) -> WorkflowState:
    print("[create] Creating sandbox")
    domain = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
    api_key = os.getenv("SANDBOX_API_KEY")
    image = os.getenv(
        "SANDBOX_IMAGE",
        "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2",
    )

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(seconds=120),
    )

    sandbox = await Sandbox.create(
        image,
        connection_config=config,
    )

    print(f"[create] Sandbox ready: {sandbox.id}")

    return {**state, "sandbox": sandbox}


async def prepare_workspace(state: WorkflowState) -> WorkflowState:
    print("[prepare] Writing job files")
    sandbox = state["sandbox"]
    if sandbox is None:
        raise RuntimeError("Sandbox not initialized")

    await sandbox.files.write_file(
        "/tmp/math.py",
        "result = 137 * 42\nprint(result)\n",
    )
    await sandbox.files.write_file(
        "/tmp/notes.txt",
        "LangGraph + OpenSandbox\n",
    )

    print("[prepare] Files written")

    return state


async def run_job(state: WorkflowState) -> WorkflowState:
    attempt = state["attempt"] + 1
    max_attempts = state["max_attempts"]
    command = state.get("command") or "python3 /tmp/math.py"
    print(f"[run] Executing job (attempt {attempt}/{max_attempts})")
    sandbox = state["sandbox"]
    if sandbox is None:
        raise RuntimeError("Sandbox not initialized")

    execution = await sandbox.commands.run(command)
    run_output = _format_execution(execution)
    last_error = ""
    next_command = command

    if execution.error:
        last_error = f"{execution.error.name}: {execution.error.value}"
        if attempt < max_attempts:
            next_command = state.get("fallback_command", "python /tmp/math.py")
            print(f"[run] Failed, scheduling fallback: {next_command}")

    print(f"[run] Output: {run_output}")

    return {
        **state,
        "run_output": run_output,
        "last_error": last_error,
        "attempt": attempt,
        "command": next_command,
    }


def decide_next(state: WorkflowState) -> str:
    if state.get("last_error") and state["attempt"] < state["max_attempts"]:
        print("[decide] Retry with fallback command")
        return "run"

    print("[decide] Proceeding to inspect")
    return "inspect"


async def inspect_results(state: WorkflowState) -> WorkflowState:
    print("[inspect] Reading notes and summarizing")
    sandbox = state["sandbox"]
    if sandbox is None:
        raise RuntimeError("Sandbox not initialized")

    notes = await sandbox.files.read_file("/tmp/notes.txt")
    llm = _build_llm()
    prompt = (
        "Summarize the sandbox run result and notes in one sentence. "
        f"Run output: {state.get('run_output', '')}. "
        f"Notes: {notes.strip()}."
    )
    response = await llm.ainvoke(prompt)

    print(f"[inspect] Summary: {response.content}")

    return {**state, "summary": response.content}


async def cleanup_sandbox(state: WorkflowState) -> WorkflowState:
    print("[cleanup] Cleaning up sandbox")
    sandbox = state.get("sandbox")
    if sandbox is not None:
        await sandbox.kill()
        await sandbox.close()

    print("[cleanup] Done")

    return {**state, "sandbox": None, "cleaned": True}


async def main() -> None:
    graph = StateGraph(WorkflowState)
    graph.add_node("create", create_sandbox)
    graph.add_node("prepare", prepare_workspace)
    graph.add_node("run", run_job)
    graph.add_node("inspect", inspect_results)
    graph.add_node("cleanup", cleanup_sandbox)
    graph.set_entry_point("create")
    graph.add_edge("create", "prepare")
    graph.add_edge("prepare", "run")
    graph.add_conditional_edges(
        "run",
        decide_next,
        {
            "run": "run",
            "inspect": "inspect",
        },
    )
    graph.add_edge("inspect", "cleanup")
    graph.add_edge("cleanup", END)
    app = graph.compile()

    initial_state = {
        "sandbox": None,
        "run_output": "",
        "summary": "",
        "last_error": "",
        "attempt": 0,
        "max_attempts": 2,
        "command": "python3 /tmp/math.py",
        "fallback_command": "python /tmp/math.py",
        "cleaned": False,
    }

    state = initial_state
    try:
        async for update in app.astream(initial_state, stream_mode="values"):
            state = update
    finally:
        if not state.get("cleaned"):
            sandbox = state.get("sandbox")
            if sandbox is not None:
                await sandbox.kill()
                await sandbox.close()

    print(f"Run output: {state['run_output']}")
    print(f"Summary: {state['summary']}")


if __name__ == "__main__":
    import asyncio

    asyncio.run(main())
