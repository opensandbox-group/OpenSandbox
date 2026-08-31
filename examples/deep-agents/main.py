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

"""Deep Agents + OpenSandbox example.

Runs a Deep Agent whose file and shell tools execute inside an OpenSandbox
sandbox. The `langchain-sandbox-opensandbox` package adapts the OpenSandbox
Python SDK to the Deep Agents `BaseSandbox` interface, so every file read/write
and command the agent runs is sandboxed.

Prerequisites:
    pip install deepagents langchain-sandbox-opensandbox

Environment:
    SANDBOX_DOMAIN     Host (and optional port) of the OpenSandbox server.
                       Defaults to "localhost:8080".
    SANDBOX_PROTOCOL   "http" (default) or "https".
    SANDBOX_API_KEY    API key, if the server requires authentication.
    ANTHROPIC_API_KEY  Required by the default Deep Agents model.
"""

import os

from deepagents import create_deep_agent
from langchain_opensandbox import OpenSandboxBackend
from opensandbox import SandboxSync
from opensandbox.config.connection_sync import ConnectionConfigSync


def main() -> None:
    connection = ConnectionConfigSync(
        domain=os.getenv("SANDBOX_DOMAIN", "localhost:8080"),
        api_key=os.getenv("SANDBOX_API_KEY"),
        protocol=os.getenv("SANDBOX_PROTOCOL", "http"),
    )
    sandbox = SandboxSync.create("python:3.12", connection_config=connection)
    backend = OpenSandboxBackend(sandbox=sandbox, timeout=300)

    try:
        agent = create_deep_agent(
            tools=[],
            system_prompt=(
                "You are a coding assistant. Use the sandbox to write and run "
                "Python, and verify your work by executing it."
            ),
            backend=backend,
        )

        result = agent.invoke(
            {
                "messages": [
                    {
                        "role": "user",
                        "content": (
                            "Write a script that prints the first 10 Fibonacci "
                            "numbers, save it as fib.py, run it, and report the "
                            "output."
                        ),
                    }
                ]
            }
        )

        print(result["messages"][-1].content)
    finally:
        sandbox.destroy()


if __name__ == "__main__":
    main()
