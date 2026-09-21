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

import time
from datetime import timedelta

import requests
from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.sandboxes import NetworkPolicy, NetworkRule


def check_nullclaw(sbx: SandboxSync) -> bool:
    """
    Health check: poll nullclaw gateway until it returns 200.

    Returns:
        True  when ready
        False on timeout or any exception
    """
    try:
        endpoint = sbx.get_endpoint(3000)
        start = time.perf_counter()
        url = f"http://{endpoint.endpoint}/health"
        for _ in range(150):  # max for ~30s
            try:
                resp = requests.get(url, timeout=1)
                if resp.status_code == 200:
                    elapsed = time.perf_counter() - start
                    print(f"[check] sandbox ready after {elapsed:.1f}s")
                    return True
            except Exception:
                pass
            time.sleep(0.2)
        return False
    except Exception as exc:
        print(f"[check] failed: {exc}")
        return False


def main() -> None:
    server = "http://localhost:8080"
    image = "ghcr.io/nullclaw/nullclaw:latest"
    timeout_seconds = 3600  # 1 hour

    print(f"Creating nullclaw sandbox with image={image} on OpenSandbox server {server}...")
    sandbox = SandboxSync.create(
        image=image,
        timeout=timedelta(seconds=timeout_seconds),
        metadata={"example": "nullclaw"},
        connection_config=ConnectionConfigSync(domain=server),
        health_check=check_nullclaw,
        # use network policy to limit nullclaw network accesses
        network_policy=NetworkPolicy(
            defaultAction="deny",
            egress=[NetworkRule(action="allow", target="openrouter.ai")],
        ),
    )

    endpoint = sandbox.get_endpoint(3000)
    print(f"Nullclaw gateway started. Please refer to {endpoint.endpoint}")


if __name__ == "__main__":
    main()
