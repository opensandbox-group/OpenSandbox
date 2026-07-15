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

"""
Windows sandbox example with ENI CNI network fix.

Use this example on clusters with ENI-based CNIs (e.g. Alibaba Cloud ACK
Terway in ENI mode) where dockur/windows fails with:

    ERROR: This container does not support host mode networking!

or:

    ERROR: Status 1 while: ethtool -i "$VM_NET_DEV"

The fix patches /run/network.sh at container startup to bypass the
ethtool/bus-info check, then uses NETWORK=slirp for QEMU user-mode NAT.
Standard veth-based CNIs (Calico, Flannel, Cilium) do NOT need this fix.
"""

import os
from datetime import timedelta

from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync
from opensandbox.models.sandboxes import PlatformSpec

# sed command to bypass the ethtool/grep checks in network.sh.
# Replaces three lines with empty variable assignments so that:
# - ethtool -i (would fail on ENI with real PCI bus-info) is skipped
# - grep on empty result (would fail with pipefail) is skipped
_NETWORK_PATCH_CMD = (
    "sed -i"
    " -e 's/result=$(ethtool -i \"$VM_NET_DEV\")/result=\"\"/'"
    " -e '/grep.*driver:/s/.*/  nic=\"\"/'"
    " -e '/grep.*bus-info:/s/.*/  bus=\"\"/'"
    " /run/network.sh"
)

# Original dockur/windows ENTRYPOINT
_WINDOWS_ENTRYPOINT = "/usr/bin/tini -s /run/entry.sh"


def main() -> None:
    cfg = ConnectionConfigSync(
        domain=os.getenv("SANDBOX_DOMAIN", "localhost:8080"),
        api_key=os.getenv("SANDBOX_API_KEY") or None,
        request_timeout=timedelta(minutes=3),
        use_server_proxy=True,
    )

    sbx = SandboxSync.create(
        image="dockurr/windows:latest",
        timeout=timedelta(hours=12),
        ready_timeout=timedelta(minutes=120),
        resource={"cpu": "8", "memory": "16G", "disk": "64G"},
        env={
            "VERSION": "11",
            "NETWORK": "slirp",  # Use QEMU built-in user-mode NAT
        },
        # Patch network.sh then exec the original entrypoint
        entrypoint=["/bin/sh", "-c", f"{_NETWORK_PATCH_CMD} && exec {_WINDOWS_ENTRYPOINT}"],
        platform=PlatformSpec(os="windows", arch="amd64"),
        connection_config=cfg,
    )

    try:
        print(f"Created: {sbx.id}")
        print(f"execd:    {sbx.get_endpoint(44772).endpoint}")
        print(f"RDP:      {sbx.get_endpoint(3389).endpoint}")
        print(f"Web:      {sbx.get_endpoint(8006).endpoint}")

        result = sbx.commands.run("cmd /c echo Hello from Windows sandbox")
        print(f"Command output: {result.logs.stdout[0].text}")
    finally:
        sbx.kill()
        sbx.close()


if __name__ == "__main__":
    main()
