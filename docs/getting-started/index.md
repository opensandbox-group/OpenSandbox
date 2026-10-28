---
title: Quick Start
description: Get OpenSandbox running locally in minutes with Docker and Python.
---

# Quick Start

OpenSandbox is a general-purpose sandbox platform for AI applications, offering multi-language SDKs, unified sandbox APIs, and Docker/Kubernetes runtimes.

## Prerequisites

- **Docker** Engine 20.10+ (for local execution)
- **Python** 3.10+ (for the server and Python SDK)
- **uv** (recommended) or pip

## 1. Start the Server

```bash
# Generate a starter config
uvx opensandbox-server init-config ~/.sandbox.toml --example docker

# Start the server
uvx opensandbox-server
```

Verify the server is running:

```bash
curl http://127.0.0.1:8080/health
# → {"status": "healthy"}
```

## 2. Install an SDK

::: code-group

```bash [Python]
pip install opensandbox
```

```bash [JavaScript]
npm install @alibaba-group/opensandbox
```

```bash [Go]
go get github.com/alibaba/OpenSandbox/sdks/sandbox/go
```

```bash [C#]
dotnet add package Alibaba.OpenSandbox
```

:::

For Kotlin/Java, see [Installation](/getting-started/installation).

## 3. Create and Use a Sandbox

This example uses the sandbox SDK and a standard Python image. If the server has an API key configured, set `OPEN_SANDBOX_API_KEY` in your client environment.

```python
import asyncio
from datetime import timedelta

from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig
from opensandbox.models import WriteEntry


async def main() -> None:
    sandbox = await Sandbox.create(
        "python:3.12",
        timeout=timedelta(minutes=10),
        connection_config=ConnectionConfig(
            domain="localhost:8080",
            use_server_proxy=True,
        ),
    )
    try:
        execution = await sandbox.commands.run("python -c 'print(1 + 1)'")
        for output in execution.logs.stdout:
            print(output.text)

        await sandbox.files.write_files([
            WriteEntry(path="/tmp/hello.txt", data="Hello World", mode=644)
        ])
        print(await sandbox.files.read_file("/tmp/hello.txt"))
    finally:
        await sandbox.destroy()


if __name__ == "__main__":
    asyncio.run(main())
```

`use_server_proxy=True` routes sandbox traffic through the lifecycle server, which is useful when the client cannot reach container addresses directly. `destroy()` terminates the remote sandbox and closes local SDK resources. Closing the client alone does not terminate the remote sandbox.

## 4. Try the CLI

```bash
pip install opensandbox-cli

osb config init
osb config set connection.domain localhost:8080
osb config set connection.protocol http
osb config set connection.use_server_proxy true
osb sandbox create --image python:3.12 --timeout 30m -o json
osb command run <sandbox-id> -o raw --argv -- python -c "print(1 + 1)"
osb sandbox kill <sandbox-id> -o json
```

## Next Steps

- [Installation](/getting-started/installation) — Detailed installation for all SDKs and runtimes
- [Configuration](/getting-started/configuration) — Server configuration reference
- [Architecture](/architecture/) — How OpenSandbox works under the hood
- [Guides](/guides/) — Client Pool, tracing, diagnostics, and security features
- [SDK capability matrix](/sdks/#capability-coverage) — Check support before choosing a language
- [Examples](/examples/) — Real-world usage examples
