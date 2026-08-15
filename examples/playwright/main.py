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
from pathlib import Path

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig


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
        "opensandbox/playwright:latest",
    )
    python_version = os.getenv("PYTHON_VERSION", "3.11")

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(seconds=60),
    )

    # Inject Python version and target URL into the container environment
    env = {
        "PYTHON_VERSION": python_version,
        "TARGET_URL": os.getenv("TARGET_URL", "https://example.com"),
    }
    sandbox = await Sandbox.create(
        image,
        connection_config=config,
        env=env,
    )

    async with sandbox:
        # Playwright and Chromium are pre-installed in the image
        # Run browser script
        browse_exec = await sandbox.commands.run(
            "python - <<'PY'\n"
            "import asyncio\n"
            "import os\n"
            "from pathlib import Path\n"
            "from playwright.async_api import async_playwright\n"
            "\n"
            "URL = os.environ.get('TARGET_URL', 'https://example.com')\n"
            "SCREENSHOT_PATH = Path('/home/playwright/screenshot.png')\n"
            "SCREENSHOT_PATH.parent.mkdir(parents=True, exist_ok=True)\n"
            "\n"
            "async def run():\n"
            "    async with async_playwright() as p:\n"
            "        browser = await p.chromium.launch(headless=True)\n"
            "        page = await browser.new_page()\n"
            "        await page.goto(URL, wait_until='networkidle')\n"
            "        title = await page.title()\n"
            "        content = await page.text_content('body')\n"
            "        await page.screenshot(path=str(SCREENSHOT_PATH), full_page=True)\n"
            "        print('title:', title)\n"
            "        print('screenshot saved at:', SCREENSHOT_PATH)\n"
            "        if content:\n"
            "            snippet = content.strip().replace('\\n', ' ')\n"
            "            print('content snippet:', snippet[:300])\n"
            "        await browser.close()\n"
            "\n"
            "asyncio.run(run())\n"
            "PY"
        )
        await _print_logs("browse", browse_exec)

        # Download screenshot from sandbox to local disk
        screenshot_remote = "/home/playwright/screenshot.png"
        screenshot_local = Path("screenshot.png")
        try:
            data = await sandbox.files.read_bytes(screenshot_remote)
            screenshot_local.write_bytes(data)
            print(f"\nDownloaded screenshot to: {screenshot_local.resolve()}")
        except Exception as e:
            print(f"\nFailed to download screenshot from {screenshot_remote}: {e}")

        await sandbox.kill()


if __name__ == "__main__":
    asyncio.run(main())
