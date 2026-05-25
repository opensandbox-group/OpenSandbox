# Copyright 2026 Alibaba Group Holding Ltd.
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
import time
from datetime import timedelta

from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule
import requests


# Configuration defaults - can be overridden via environment variables
DEFAULT_SERVER = os.getenv("OPENCLAW_SERVER", "http://localhost:8080")
DEFAULT_IMAGE = os.getenv("OPENCLAW_IMAGE", "ghcr.io/openclaw/openclaw:latest")
DEFAULT_TIMEOUT = int(os.getenv("OPENCLAW_TIMEOUT", "3600"))
DEFAULT_TOKEN = os.getenv("OPENCLAW_TOKEN", "dummy-token-for-sandbox")
DEFAULT_PORT = int(os.getenv("OPENCLAW_PORT", "18789"))


def check_openclaw(sbx: SandboxSync, port: int = DEFAULT_PORT) -> bool:
    """
    Health check: poll openclaw until it returns 200.

    Args:
        sbx: SandboxSync instance
        port: Gateway port to check

    Returns:
        True  when ready
        False on timeout or any exception
    """
    try:
        endpoint = sbx.get_endpoint(port)
        start = time.perf_counter()
        url = f"http://{endpoint.endpoint}"
        for _ in range(150):  # max for ~30s
            try:
                resp = requests.get(url, timeout=1)
                if resp.status_code == 200:
                    elapsed = time.perf_counter() - start
                    print(f"[check] sandbox ready after {elapsed:.1f}s")
                    return True
            except Exception as exc:
                pass
            time.sleep(0.2)
        return False
    except Exception as exc:
        print(f"[check] failed: {exc}")
        return False


def main() -> None:
    server = DEFAULT_SERVER
    image = DEFAULT_IMAGE
    timeout_seconds = DEFAULT_TIMEOUT
    token = os.getenv("OPENCLAW_GATEWAY_TOKEN", DEFAULT_TOKEN)
    port = DEFAULT_PORT

    print(f"Creating openclaw sandbox with image={image} on OpenSandbox server {server}...")
    print(f"  Token: {token[:16]}..." if len(token) > 16 else f"  Token: {token}")
    print(f"  Port: {port}")
    print(f"  Timeout: {timeout_seconds}s")
    
    sandbox = SandboxSync.create(
        image=image,
        timeout=timedelta(seconds=timeout_seconds),
        metadata={"example": "openclaw"},
        entrypoint=[f"node dist/index.js gateway --bind=lan --port {port} --allow-unconfigured --verbose"],
        connection_config=ConnectionConfigSync(domain=server),
        health_check=lambda sbx: check_openclaw(sbx, port),
        # env for openclaw
        env={
            "OPENCLAW_GATEWAY_TOKEN": token
        },
        # use network policy to limit openclaw network accesses
        network_policy=NetworkPolicy(
            defaultAction="deny",
            egress=[
                NetworkRule(action="allow", target="pypi.org"),
                NetworkRule(action="allow", target="pypi.python.org"),
                NetworkRule(action="allow", target="github.com"),
                NetworkRule(action="allow", target="api.github.com"),
            ],
        ),
    )

    endpoint = sandbox.get_endpoint(port)
    print(f"Openclaw started finished. Please refer to {endpoint.endpoint}")

if __name__ == "__main__":
    main()