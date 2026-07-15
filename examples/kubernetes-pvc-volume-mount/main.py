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

#!/usr/bin/env python3
"""
Kubernetes PVC Volume Mount Example

This example demonstrates how to use PersistentVolumeClaims (PVC) with OpenSandbox
in a Kubernetes environment. It verifies that data written to a PVC persists across
sandbox lifecycles.

Prerequisites:
1. Kubernetes cluster with CSI driver installed
2. PVC created in the target namespace
3. OpenSandbox server running with Kubernetes runtime

Usage:
    export OPEN_SANDBOX_API_KEY=your-api-key
    export OPEN_SANDBOX_BASE_URL=http://localhost:8080
    python main.py
"""

import asyncio
import os
from datetime import timedelta
from opensandbox import Sandbox
from opensandbox.models.sandboxes import PVC, Volume
from opensandbox.config.connection import ConnectionConfig

# Configuration
PVC_NAME = os.getenv("SANDBOX_PVC_NAME", "my-pvc")
IMAGE = os.getenv("SANDBOX_IMAGE", "python:3.11")

# Connection config with extended timeout for sandbox creation
CONNECTION_CONFIG = ConnectionConfig(
    request_timeout=timedelta(minutes=10),
)

VOLUMES = [
    Volume(
        name="data-volume",
        pvc=PVC(claimName=PVC_NAME),
        mountPath="/mnt/data",
        readOnly=False,
    ),
]


async def basic_pvc_mount():
    print("\n" + "=" * 60)
    print("Step 1: Create sandbox and write data to PVC")
    print("=" * 60)
    print(f"  PVC name  : {PVC_NAME}")
    print(f"  Mount path: /mnt/data")
    print()

    sandbox = await Sandbox.create(
        image=IMAGE,
        timeout=timedelta(minutes=10),
        ready_timeout=timedelta(minutes=10),
        volumes=VOLUMES,
        connection_config=CONNECTION_CONFIG,
    )
    print(f"  Created sandbox: {sandbox.id}")

    # Write a test file to PVC
    await sandbox.commands.run(
        "python -c \"with open('/mnt/data/sandbox-test.txt', 'w') as f: f.write('Hello from OpenSandbox!')\""
    )
    print("  Written test file to /mnt/data/sandbox-test.txt")

    # Read it back
    result = await sandbox.commands.run("cat /mnt/data/sandbox-test.txt")
    content = "\n".join(msg.text for msg in result.logs.stdout)
    print(f"  Read back: {content.strip()}")

    # Kill the sandbox
    await sandbox.kill()
    print("  Sandbox killed.")

    # ---- Verify persistence ----
    print("\n" + "=" * 60)
    print("Step 2: Create new sandbox and verify data persistence")
    print("=" * 60)

    sandbox2 = await Sandbox.create(
        image=IMAGE,
        timeout=timedelta(minutes=10),
        ready_timeout=timedelta(minutes=10),
        volumes=VOLUMES,
        connection_config=CONNECTION_CONFIG,
    )
    print(f"  Created new sandbox: {sandbox2.id}")

    # Read the file written by the previous sandbox
    result = await sandbox2.commands.run("cat /mnt/data/sandbox-test.txt")
    content = "\n".join(msg.text for msg in result.logs.stdout).strip()
    print(f"  Read back from new sandbox: {content}")

    if content == "Hello from OpenSandbox!":
        print("  Data persistence verified!")
    else:
        print(f"  ERROR: Expected 'Hello from OpenSandbox!', got '{content}'")

    await sandbox2.kill()
    print("  Sandbox killed.")


async def main():
    """Main entry point."""
    print("\n" + "=" * 60)
    print("OpenSandbox Kubernetes PVC Volume Mount Example")
    print("=" * 60)
    print(f"PVC Name   : {PVC_NAME}")
    print(f"Image      : {IMAGE}")
    print()

    try:
        await basic_pvc_mount()

        print("\n" + "=" * 60)
        print("All steps completed successfully!")
        print("=" * 60)

    except Exception as e:
        print(f"\nError: {e}")
        raise


if __name__ == "__main__":
    asyncio.run(main())
