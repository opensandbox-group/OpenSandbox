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
Docker OSSFS Volume Mount Example
=================================

Demonstrates how to create OSSFS volumes with the new SDK model and mount them
into sandboxes on Docker runtime.

Scenarios:
1) Basic read-write mount on OSSFS backend.
2) Cross-sandbox data sharing on same OSSFS backend path.
3) Two volumes use different OSS prefixes via subPath.
"""

import asyncio
import os
from datetime import timedelta
from uuid import uuid4

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig

try:
    from opensandbox.models.sandboxes import OSSFS, Volume
except ImportError:
    print(
        "ERROR: Your installed opensandbox SDK does not include OSSFS/Volume models.\n"
        "       Please install the latest SDK from source:\n"
        "\n"
        "           pip install -e sdks/sandbox/python\n"
    )
    raise SystemExit(1)


def _required_env(name: str) -> str:
    value = os.getenv(name, "").strip()
    if not value:
        raise RuntimeError(f"Missing required environment variable: {name}")
    return value


def build_ossfs() -> OSSFS:
    return OSSFS(
        bucket=_required_env("OSS_BUCKET"),
        endpoint=_required_env("OSS_ENDPOINT"),
        accessKeyId=_required_env("OSS_ACCESS_KEY_ID"),
        accessKeySecret=_required_env("OSS_ACCESS_KEY_SECRET"),
    )


async def print_exec(sandbox: Sandbox, command: str) -> str:
    result = await sandbox.commands.run(command)
    stdout = "\n".join(msg.text for msg in result.logs.stdout).strip()
    stderr = "\n".join(msg.text for msg in result.logs.stderr).strip()
    if stdout:
        print(stdout)
    if stderr:
        print(stderr)
    if result.error:
        raise RuntimeError(f"Command failed: {result.error.name}: {result.error.value}")
    return stdout


async def demo_basic_mount(config: ConnectionConfig, image: str, run_id: str) -> None:
    print("\n" + "=" * 60)
    print("Scenario 1: Basic OSSFS Read-Write Mount")
    print("=" * 60)
    sandbox = await Sandbox.create(
        image=image,
        connection_config=config,
        timeout=timedelta(minutes=3),
        volumes=[
            Volume(
                name="oss-root",
                ossfs=build_ossfs(),
                mountPath="/mnt/oss",
                readOnly=False,
            )
        ],
    )
    async with sandbox:
        try:
            await print_exec(sandbox, "mkdir -p /mnt/oss/opensandbox-demo")
            await print_exec(
                sandbox,
                f"echo 'hello-{run_id}' > /mnt/oss/opensandbox-demo/basic.txt",
            )
            print("[verify] read file from mounted OSSFS path:")
            await print_exec(sandbox, "cat /mnt/oss/opensandbox-demo/basic.txt")
        finally:
            await sandbox.kill()


async def demo_cross_sandbox_sharing(config: ConnectionConfig, image: str, run_id: str) -> None:
    print("\n" + "=" * 60)
    print("Scenario 2: Cross-Sandbox Sharing")
    print("=" * 60)
    writer = await Sandbox.create(
        image=image,
        connection_config=config,
        timeout=timedelta(minutes=3),
        volumes=[
            Volume(
                name="oss-root-writer",
                ossfs=build_ossfs(),
                mountPath="/mnt/oss",
            )
        ],
    )
    async with writer:
        try:
            await print_exec(
                writer,
                f"echo 'from-writer-{run_id}' > /mnt/oss/opensandbox-demo/shared.txt",
            )
        finally:
            await writer.kill()

    reader = await Sandbox.create(
        image=image,
        connection_config=config,
        timeout=timedelta(minutes=3),
        volumes=[
            Volume(
                name="oss-root-reader",
                ossfs=build_ossfs(),
                mountPath="/mnt/oss",
                readOnly=True,
            )
        ],
    )
    async with reader:
        try:
            print("[verify] sandbox B reads file created by sandbox A:")
            await print_exec(reader, "cat /mnt/oss/opensandbox-demo/shared.txt")
        finally:
            await reader.kill()


async def demo_subpath_mounts(config: ConnectionConfig, image: str, run_id: str) -> None:
    print("\n" + "=" * 60)
    print("Scenario 3: Different OSS Prefixes via subPath")
    print("=" * 60)
    setup = await Sandbox.create(
        image=image,
        connection_config=config,
        timeout=timedelta(minutes=3),
        volumes=[
            Volume(
                name="oss-root-setup",
                ossfs=build_ossfs(),
                mountPath="/mnt/oss",
            )
        ],
    )
    async with setup:
        try:
            await print_exec(
                setup,
                "mkdir -p /mnt/oss/opensandbox-demo/subpath-a /mnt/oss/opensandbox-demo/subpath-b",
            )
        finally:
            await setup.kill()

    sandbox = await Sandbox.create(
        image=image,
        connection_config=config,
        timeout=timedelta(minutes=3),
        volumes=[
            Volume(
                name="oss-a",
                ossfs=build_ossfs(),
                mountPath="/mnt/a",
                subPath="opensandbox-demo/subpath-a",
            ),
            Volume(
                name="oss-b",
                ossfs=build_ossfs(),
                mountPath="/mnt/b",
                subPath="opensandbox-demo/subpath-b",
            ),
        ],
    )
    async with sandbox:
        try:
            await print_exec(sandbox, f"echo 'A-{run_id}' > /mnt/a/file.txt")
            await print_exec(sandbox, f"echo 'B-{run_id}' > /mnt/b/file.txt")
            print("[verify] subPath A content:")
            await print_exec(sandbox, "cat /mnt/a/file.txt")
            print("[verify] subPath B content:")
            await print_exec(sandbox, "cat /mnt/b/file.txt")
        finally:
            await sandbox.kill()


async def main() -> None:
    domain = os.getenv("SANDBOX_DOMAIN", "localhost:8080")
    api_key = os.getenv("SANDBOX_API_KEY")
    image = os.getenv("SANDBOX_IMAGE", "ubuntu")
    run_id = uuid4().hex[:8]

    config = ConnectionConfig(
        domain=domain,
        api_key=api_key,
        request_timeout=timedelta(minutes=5),
    )

    print(f"OpenSandbox server : {domain}")
    print(f"Sandbox image      : {image}")
    print(f"OSS bucket         : {_required_env('OSS_BUCKET')}")
    print(f"OSS endpoint       : {_required_env('OSS_ENDPOINT')}")

    await demo_basic_mount(config, image, run_id)
    await demo_cross_sandbox_sharing(config, image, run_id)
    await demo_subpath_mounts(config, image, run_id)

    print("\n" + "=" * 60)
    print("All OSSFS scenarios completed successfully.")
    print("=" * 60)


if __name__ == "__main__":
    asyncio.run(main())
