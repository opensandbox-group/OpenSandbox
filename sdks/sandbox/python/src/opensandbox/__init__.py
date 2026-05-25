#
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
#
"""
OpenSandbox Python SDK

Secure, isolated execution environments for code and applications.

## Basic Usage

```python
import asyncio
from opensandbox import Sandbox
from opensandbox.models.execd import RunCommandOpts
from opensandbox.models.sandboxes import SandboxImageSpec

async def main():
    # Create a sandbox instance.
    #
    # Note on lifecycle:
    # - Exiting the context manager will call `sandbox.close()` (local HTTP resources only).
    # - You must still call `sandbox.kill()` to terminate the remote sandbox instance.
    async with await Sandbox.create("python:3.11") as sandbox:
        # Write a file
        await sandbox.files.write_file("hello.py", "print('Hello World')")

        # Execute a command
        result = await sandbox.commands.run("python hello.py")
        print(result.logs.stdout[0].text)  # Hello World

if __name__ == "__main__":
    asyncio.run(main())
```

## Advanced Usage

```python
from datetime import timedelta
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models.execd import RunCommandOpts
from opensandbox.models.sandboxes import SandboxImageSpec, SandboxImageAuth

async def main():
    config = ConnectionConfig(
        api_key="your-api-key",
        domain="api.opensandbox.io"
    )

    # With private registry auth
    image_spec = SandboxImageSpec(
        "my-registry.com/python:3.11",
        auth=SandboxImageAuth(username="user", password="secret")
    )

    sandbox = await Sandbox.create(
        image_spec,
        timeout=timedelta(minutes=30),
        env={"PYTHONPATH": "/workspace"},
        connection_config=config,
    )

    try:
        # File operations
        await sandbox.files.write_file("script.py", "print('Hello OpenSandbox!')")

        # Command execution
        result = await sandbox.commands.run("python script.py")
        print(result.logs.stdout[0].text)

        # Get metrics
        metrics = await sandbox.get_metrics()
        print(f"Memory usage: {metrics.memory_used_in_mib}MB")

    finally:
        await sandbox.kill()
        await sandbox.close()

if __name__ == "__main__":
    asyncio.run(main())
```

For advanced code execution with persistent contexts, see the separate
`opensandbox-code-interpreter` package.
"""

from importlib.metadata import PackageNotFoundError
from importlib.metadata import version as _pkg_version

from opensandbox.manager import SandboxManager
from opensandbox.pool import (
    AcquirePolicy,
    AsyncSandboxPool,
    InMemoryAsyncPoolStateStore,
    InMemoryPoolStateStore,
    PoolCreationSpec,
    SandboxPool,
    SandboxPoolAsync,
)
from opensandbox.sandbox import Sandbox
from opensandbox.sync import SandboxManagerSync, SandboxPoolSync, SandboxSync

try:
    __version__ = _pkg_version("opensandbox")
except PackageNotFoundError:  # pragma: no cover
    # Fallback for editable/uninstalled source checkouts.
    __version__ = "0.0.0"

__all__ = [
    "Sandbox",
    "SandboxManager",
    "SandboxSync",
    "SandboxManagerSync",
    "SandboxPoolSync",
    "SandboxPoolAsync",
    "AsyncSandboxPool",
    "SandboxPool",
    "AcquirePolicy",
    "PoolCreationSpec",
    "InMemoryAsyncPoolStateStore",
    "InMemoryPoolStateStore",
]
