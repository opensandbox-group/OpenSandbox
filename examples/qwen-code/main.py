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
import json
import os
from datetime import timedelta

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models.filesystem import WriteEntry


QWEN_PROJECT_DIR = "/tmp/qwen-code-example"
QWEN_SETTINGS_DIR = f"{QWEN_PROJECT_DIR}/.qwen"
QWEN_SETTINGS_PATH = f"{QWEN_SETTINGS_DIR}/settings.json"


def _required_env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise RuntimeError(f"{name} is required")
    return value


def _build_qwen_settings(base_url: str, model_name: str) -> str:
    settings = {
        "modelProviders": {
            "openai": [
                {
                    "id": model_name,
                    "name": model_name,
                    "baseUrl": base_url,
                    "description": "Qwen Code via OpenAI-compatible API in OpenSandbox",
                    "envKey": "API_KEY",
                }
            ]
        },
        "security": {
            "auth": {
                "selectedType": "openai",
            }
        },
        "model": {
            "name": model_name,
        },
    }
    return json.dumps(settings, indent=2)


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
    qwen_api_key = _required_env("API_KEY")
    qwen_base_url = os.getenv("BASE_URL", "https://dashscope.aliyuncs.com/compatible-mode/v1")
    qwen_model_name = os.getenv("MODEL_NAME", "qwen3-coder-plus")
    image = os.getenv(
        "SANDBOX_IMAGE",
        "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2",
    )

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(seconds=60),
    )

    sandbox = await Sandbox.create(
        image,
        connection_config=config,
        env={"API_KEY": qwen_api_key},
    )

    async with sandbox:
        await sandbox.files.create_directories(
            [
                WriteEntry(path=QWEN_PROJECT_DIR, mode=755),
                WriteEntry(path=QWEN_SETTINGS_DIR, mode=755),
            ]
        )
        await sandbox.files.write_file(
            QWEN_SETTINGS_PATH,
            _build_qwen_settings(qwen_base_url, qwen_model_name),
            mode=644,
        )

        # Install Qwen Code CLI (Node.js is already in the code-interpreter image).
        install_exec = await sandbox.commands.run(
            "npm install -g @qwen-code/qwen-code@latest"
        )
        await _print_execution_logs(install_exec)

        # Run Qwen Code in headless mode using the project-local config.
        run_exec = await sandbox.commands.run(
            'cd /tmp/qwen-code-example && qwen -p "Compute 1+1 and reply with only the final number."'
        )
        await _print_execution_logs(run_exec)

        await sandbox.kill()


if __name__ == "__main__":
    asyncio.run(main())
