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

"""Windows sandbox example using a pre-warmed K8s pool."""

import os
from datetime import timedelta

from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync


def main() -> None:
    cfg = ConnectionConfigSync(
        domain=os.getenv("SANDBOX_DOMAIN", "localhost:8080"),
        api_key=os.getenv("SANDBOX_API_KEY") or None,
        request_timeout=timedelta(minutes=3),
        use_server_proxy=True,
    )

    # Note: do NOT set entrypoint or env for Windows pool sandboxes.
    # The pool template already configures the Windows guest (VERSION,
    # CPU_CORES, etc.). Setting entrypoint or env would inject a
    # taskTemplate that overrides the pool's pod spec, preventing
    # dockur/windows from booting correctly.
    sbx = SandboxSync.create(
        image="dockurr/windows:latest",
        timeout=timedelta(hours=1),
        extensions={"poolRef": "pool-win-example"},
        connection_config=cfg,
    )

    try:
        print(f"Created: {sbx.id}")
        print(f"execd:    {sbx.get_endpoint(44772).endpoint}")
        print(f"RDP:      {sbx.get_endpoint(3389).endpoint}")
        print(f"Web:      {sbx.get_endpoint(8006).endpoint}")

        exec = sbx.commands.run("cmd /c echo Hello from Windows sandbox")
        print(f"Command output: {exec.logs.stdout[0].text}")
    finally:
        sbx.kill()
        sbx.close()


if __name__ == "__main__":
    main()
