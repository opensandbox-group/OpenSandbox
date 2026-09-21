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

"""
Create an AIO sandbox via OpenSandbox SDK, then connect to it with agent-sandbox SDK.

This example is intentionally hard-coded for simplicity:
- OpenSandbox server: http://localhost:8080
- Image: ghcr.io/agent-infra/sandbox:latest
- AIO port: 8080
- Timeout: 300s
"""

import time
from datetime import timedelta

import requests
from agent_sandbox import Sandbox as AioSandboxClient
from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync


def check_aio_process(sbx: SandboxSync) -> bool:
    """
    Health check: poll aio process at /v1/shell/sessions until it returns 200.

    Returns:
        True  when ready
        False on timeout or any exception
    """
    try:
        endpoint = sbx.get_endpoint(8080)
        start = time.perf_counter()
        url = f"http://{endpoint.endpoint}/v1/shell/sessions"
        for _ in range(150):  # max for ~30s
            try:
                resp = requests.get(url, timeout=1)
                if resp.status_code == 200:
                    elapsed = time.perf_counter() - start
                    print(f"[check] sandbox ready after {elapsed:.1f}s")
                    return True
            except Exception as exc:
                # print(f"[check] aio sandbox check health failed: {exc}")
                pass
            time.sleep(0.2)
        return False
    except Exception as exc:
        print(f"[check] failed: {exc}")
        return False


def main() -> None:
    server = "http://localhost:8080"
    image = "ghcr.io/agent-infra/sandbox:latest"
    timeout_seconds = 300

    print(f"Creating AIO sandbox with image={image} on OpenSandbox server {server}...")
    sandbox = SandboxSync.create(
        image=image,
        timeout=timedelta(seconds=timeout_seconds),
        metadata={"example": "aio-sandbox"},
        entrypoint=["/opt/gem/run.sh"],
        connection_config=ConnectionConfigSync(domain=server),
        health_check=check_aio_process,
    )

    with sandbox:
        endpoint = sandbox.get_endpoint(8080)
        print(f"AIO portal endpoint: {endpoint.endpoint}")

        client = AioSandboxClient(base_url=f"http://{endpoint.endpoint}")
        home_dir = client.sandbox.get_context().home_dir

        result = client.shell.exec_command(command="ls -la", timeout=10)
        print(result.data.output)

        content = client.file.read_file(file=f"{home_dir}/.bashrc")
        print(content.data.content)

        screenshot_path = "sandbox_screenshot.png"
        with open(screenshot_path, "wb") as f:
            for chunk in client.browser.screenshot():
                f.write(chunk)
        print(f"Screenshot saved to {screenshot_path}")

        # kill sandbox finally
        sandbox.kill()


if __name__ == "__main__":
    main()
