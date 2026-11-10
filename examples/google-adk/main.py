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

import os
from datetime import timedelta

from google.adk.agents import Agent
from google.adk.apps import App
from google.adk.integrations.opensandbox import OpenSandboxEnvironment
from google.adk.runners import Runner
from google.adk.sessions.in_memory_session_service import InMemorySessionService
from google.adk.tools.environment import EnvironmentToolset
from google.adk.utils._debug_output import print_event
from google.adk.utils.context_utils import Aclosing
from google.genai import types
from opensandbox.config import ConnectionConfig


def _required_env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


async def main() -> None:
    domain = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
    api_key = os.getenv("SANDBOX_API_KEY")
    image = os.getenv(
        "SANDBOX_IMAGE",
        "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0",
    )
    model_name = os.getenv("GOOGLE_ADK_MODEL", "gemini-2.5-flash")
    if model_name.startswith("openai/"):
        from google.adk.models.lite_llm import LiteLlm

        model = LiteLlm(
            model=model_name,
            api_key=_required_env("OPENAI_API_KEY"),
            api_base=os.getenv("OPENAI_BASE_URL", "https://api.openai.com/v1"),
        )
    else:
        _required_env("GOOGLE_API_KEY")
        model = model_name

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(seconds=120),
    )

    toolset = EnvironmentToolset(
        environment=OpenSandboxEnvironment(
            image=image,
            timeout=300,
            connection_config=config,
        )
    )

    try:
        agent = Agent(
            name="opensandbox_adk",
            model=model,
            instruction=(
                "Use the environment tools to work inside the OpenSandbox workspace. "
                "Use WriteFile to create files, ReadFile to read them, EditFile to "
                "edit them, and Execute to run commands. Report actual tool output."
            ),
            tools=[toolset],
        )

        app = App(name="opensandbox_adk", root_agent=agent)
        session_service = InMemorySessionService()
        runner = Runner(app=app, session_service=session_service)
        session = await session_service.create_session(
            app_name=app.name,
            user_id="local-user",
        )

        prompts = [
            "Use WriteFile to save /workspace/math.py that prints 137 * 42.",
            "Use Execute to run python /workspace/math.py and report the result.",
            (
                "Use WriteFile to write /workspace/notes.txt with 'ADK + OpenSandbox', "
                "then use ReadFile to read it back."
            ),
        ]

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
        await toolset.close()


if __name__ == "__main__":
    import asyncio

    asyncio.run(main())
