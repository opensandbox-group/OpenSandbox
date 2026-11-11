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
Docker PVC (Named Volume) Mount Example
========================================

Demonstrates how to mount Docker named volumes into sandbox containers using
the OpenSandbox ``pvc`` backend.  In Docker runtime the ``pvc`` backend maps
``claimName`` to a Docker named volume -- providing a more convenient and
secure alternative to host-path bind mounts for sharing data across sandboxes.

Four scenarios are demonstrated:

1. **Read-write mount**        - Mount a named volume for bidirectional file I/O.
2. **Read-only mount**         - Mount a named volume as read-only.
3. **Cross-sandbox sharing**   - Two sandboxes share data through the same named
   volume without exposing any host path.
4. **SubPath mount**           - Mount only a subdirectory of a named volume,
   keeping the same API as Kubernetes PVC subPath.

Prerequisites:
- OpenSandbox server running with Docker runtime
- Docker named volume created before running this script (see README.md)
"""

import asyncio
import os
import subprocess
from datetime import timedelta

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig

try:
    from opensandbox.models.sandboxes import PVC, Volume
except ImportError:
    print(
        "ERROR: Your installed opensandbox SDK does not include Volume/PVC models.\n"
        "       Volume support requires the latest SDK from source.\n"
        "       Please install from the local repository:\n"
        "\n"
        "           pip install -e sdks/sandbox/python\n"
        "\n"
        "       See README.md for details."
    )
    raise SystemExit(1)


VOLUME_NAME = "opensandbox-pvc-demo"


async def print_exec(sandbox: Sandbox, command: str) -> str | None:
    """Run a command in the sandbox and print/return stdout."""
    result = await sandbox.commands.run(command)
    if result.error:
        print(f"  [error] {result.error.name}: {result.error.value}")
        return None
    text = "\n".join(msg.text for msg in result.logs.stdout)
    if text:
        print(f"  {text}")
    return text


def ensure_named_volume() -> None:
    """Create the Docker named volume and seed it with test data."""
    print(f"  Ensuring Docker named volume '{VOLUME_NAME}' exists...")
    subprocess.run(
        ["docker", "volume", "rm", VOLUME_NAME],
        capture_output=True,
    )
    subprocess.run(
        ["docker", "volume", "create", VOLUME_NAME],
        check=True,
        capture_output=True,
    )
    # Seed the volume with a marker file and subpath test data
    subprocess.run(
        [
            "docker", "run", "--rm",
            "-v", f"{VOLUME_NAME}:/data",
            "alpine",
            "sh", "-c",
            "echo 'hello-from-named-volume' > /data/marker.txt && "
            "mkdir -p /data/datasets/train && "
            "echo 'id,value' > /data/datasets/train/data.csv && "
            "echo '1,100' >> /data/datasets/train/data.csv && "
            "echo '2,200' >> /data/datasets/train/data.csv",
        ],
        check=True,
        capture_output=True,
    )
    print(f"  Created volume '{VOLUME_NAME}' with marker.txt and datasets/train/")


async def demo_readwrite_mount(config: ConnectionConfig, image: str) -> None:
    """
    Scenario 1: Read-write named volume mount.

    Mount a Docker named volume into the sandbox at /mnt/data.
    Write a file inside the sandbox, then read it back to verify.
    """
    print("\n" + "=" * 60)
    print("Scenario 1: Read-Write PVC (Named Volume) Mount")
    print("=" * 60)
    print(f"  Volume name: {VOLUME_NAME}")
    print(f"  Mount path : /mnt/data")

    sandbox = await Sandbox.create(
        image=image,
        connection_config=config,
        timeout=timedelta(minutes=2),
        volumes=[
            Volume(
                name="demo-data",
                pvc=PVC(claimName=VOLUME_NAME),
                mountPath="/mnt/data",
                readOnly=False,
            ),
        ],
    )

    async with sandbox:
        try:
            # Read the seeded marker file
            print("\n  [1] Reading marker file from named volume:")
            await print_exec(sandbox, "cat /mnt/data/marker.txt")

            # Write a new file
            print("\n  [2] Writing a file from inside the sandbox:")
            await print_exec(
                sandbox,
                "echo 'written-by-sandbox' > /mnt/data/sandbox-output.txt",
            )
            print("  -> Written: /mnt/data/sandbox-output.txt")

            # Read it back
            print("\n  [3] Reading back the written file:")
            await print_exec(sandbox, "cat /mnt/data/sandbox-output.txt")

            # List all files
            print("\n  [4] Listing volume contents:")
            await print_exec(sandbox, "ls -la /mnt/data/")

        finally:
            await sandbox.kill()

    print("\n  Scenario 1 completed.")


async def demo_readonly_mount(config: ConnectionConfig, image: str) -> None:
    """
    Scenario 2: Read-only named volume mount.

    Mount the same named volume as read-only.  Verify reads succeed but
    writes are rejected by the container runtime.
    """
    print("\n" + "=" * 60)
    print("Scenario 2: Read-Only PVC (Named Volume) Mount")
    print("=" * 60)
    print(f"  Volume name: {VOLUME_NAME}")
    print(f"  Mount path : /mnt/readonly")

    sandbox = await Sandbox.create(
        image=image,
        connection_config=config,
        timeout=timedelta(minutes=2),
        volumes=[
            Volume(
                name="readonly-vol",
                pvc=PVC(claimName=VOLUME_NAME),
                mountPath="/mnt/readonly",
                readOnly=True,
            ),
        ],
    )

    async with sandbox:
        try:
            # Read the marker file
            print("\n  [1] Reading marker.txt from read-only mount:")
            await print_exec(sandbox, "cat /mnt/readonly/marker.txt")

            # Attempt to write (should fail)
            print("\n  [2] Attempting to write (should fail):")
            result = await sandbox.commands.run(
                "touch /mnt/readonly/should-fail.txt 2>&1 || echo 'Write denied (expected)'"
            )
            for msg in result.logs.stdout:
                print(f"  {msg.text}")
            for msg in result.logs.stderr:
                print(f"  {msg.text}")

        finally:
            await sandbox.kill()

    print("\n  Scenario 2 completed.")


async def demo_cross_sandbox_sharing(config: ConnectionConfig, image: str) -> None:
    """
    Scenario 3: Cross-sandbox data sharing via named volume.

    Two sandboxes mount the same named volume.  Sandbox A writes a file,
    then Sandbox B reads it -- demonstrating data sharing without any host
    path exposure.
    """
    print("\n" + "=" * 60)
    print("Scenario 3: Cross-Sandbox Sharing via PVC (Named Volume)")
    print("=" * 60)
    print(f"  Volume name: {VOLUME_NAME}")

    volume_spec = Volume(
        name="shared-vol",
        pvc=PVC(claimName=VOLUME_NAME),
        mountPath="/mnt/shared",
        readOnly=False,
    )

    # --- Sandbox A: write ---
    print("\n  [Sandbox A] Creating sandbox and writing data...")
    sandbox_a = await Sandbox.create(
        image=image,
        connection_config=config,
        timeout=timedelta(minutes=2),
        volumes=[volume_spec],
    )
    async with sandbox_a:
        try:
            await print_exec(
                sandbox_a,
                "echo 'message-from-sandbox-a' > /mnt/shared/cross-sandbox.txt",
            )
            print("  [Sandbox A] Wrote /mnt/shared/cross-sandbox.txt")
        finally:
            await sandbox_a.kill()

    # --- Sandbox B: read ---
    print("\n  [Sandbox B] Creating sandbox and reading data...")
    sandbox_b = await Sandbox.create(
        image=image,
        connection_config=config,
        timeout=timedelta(minutes=2),
        volumes=[volume_spec],
    )
    async with sandbox_b:
        try:
            print("  [Sandbox B] Reading file written by Sandbox A:")
            text = await print_exec(sandbox_b, "cat /mnt/shared/cross-sandbox.txt")
            if text and "message-from-sandbox-a" in text:
                print("\n  Cross-sandbox data sharing verified!")
        finally:
            await sandbox_b.kill()

    print("\n  Scenario 3 completed.")


async def demo_subpath_mount(config: ConnectionConfig, image: str) -> None:
    """
    Scenario 4: SubPath mount on a named volume.

    Mount only a subdirectory (datasets/train) of the named volume.  The server
    resolves the volume's host-side Mountpoint via ``docker volume inspect`` and
    appends the subPath, producing a standard bind mount.  This keeps the API
    consistent with Kubernetes PVC subPath semantics.
    """
    print("\n" + "=" * 60)
    print("Scenario 4: SubPath PVC (Named Volume) Mount")
    print("=" * 60)
    print(f"  Volume name: {VOLUME_NAME}")
    print(f"  SubPath    : datasets/train")
    print(f"  Mount path : /mnt/training-data")

    sandbox = await Sandbox.create(
        image=image,
        connection_config=config,
        timeout=timedelta(minutes=2),
        volumes=[
            Volume(
                name="train-data",
                pvc=PVC(claimName=VOLUME_NAME),
                mountPath="/mnt/training-data",
                readOnly=True,
                subPath="datasets/train",
            ),
        ],
    )

    async with sandbox:
        try:
            # List contents -- should only show the subpath
            print("\n  [1] Listing mounted subpath content:")
            await print_exec(sandbox, "ls -la /mnt/training-data/")

            # Read the CSV data
            print("\n  [2] Reading data.csv:")
            await print_exec(sandbox, "cat /mnt/training-data/data.csv")

            # Verify the root marker.txt is NOT visible (we're inside datasets/train)
            print("\n  [3] Verifying volume root is NOT visible:")
            result = await sandbox.commands.run("test -f /mnt/training-data/marker.txt && echo FOUND || echo NOT-FOUND")
            text = "\n".join(msg.text for msg in result.logs.stdout)
            print(f"  marker.txt at mount root: {text}")
            if "NOT-FOUND" in text:
                print("  -> Confirmed: subPath isolation is working correctly")

        finally:
            await sandbox.kill()

    print("\n  Scenario 4 completed.")


async def main() -> None:
    domain = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
    api_key = os.getenv("SANDBOX_API_KEY")
    image = os.getenv("SANDBOX_IMAGE", "ubuntu")

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(minutes=3),
    )

    print(f"OpenSandbox server : {config.domain}")
    print(f"Sandbox image      : {image}")
    print(f"Docker volume      : {VOLUME_NAME}")

    # Ensure the named volume exists with seed data
    ensure_named_volume()

    await demo_readwrite_mount(config, image)
    await demo_readonly_mount(config, image)
    await demo_cross_sandbox_sharing(config, image)
    await demo_subpath_mount(config, image)

    print("\n" + "=" * 60)
    print("All scenarios completed successfully!")
    print("=" * 60)


if __name__ == "__main__":
    asyncio.run(main())
