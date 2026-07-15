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

```python
import asyncio
from datetime import timedelta

from code_interpreter import CodeInterpreter, SupportedLanguage
from opensandbox import Sandbox
from opensandbox.models import WriteEntry

async def main() -> None:
    # Create a sandbox with code interpreter
    sandbox = await Sandbox.create(
        "opensandbox/code-interpreter:v1.1.0",
        entrypoint=["/opt/code-interpreter/code-interpreter.sh"],
        env={"PYTHON_VERSION": "3.11"},
        timeout=timedelta(minutes=10),
    )

    async with sandbox:
        # Execute a shell command
        execution = await sandbox.commands.run("echo 'Hello OpenSandbox!'")
        print(execution.logs.stdout[0].text)

        # Write and read a file
        await sandbox.files.write_files([
            WriteEntry(path="/tmp/hello.txt", data="Hello World", mode=644)
        ])
        content = await sandbox.files.read_file("/tmp/hello.txt")
        print(f"Content: {content}")

        # Run code via the Code Interpreter
        interpreter = await CodeInterpreter.create(sandbox)
        result = await interpreter.codes.run(
            "import sys; print(sys.version); 2 + 2",
            language=SupportedLanguage.PYTHON,
        )
        print(result.result[0].text)  # 4

        await sandbox.kill()

if __name__ == "__main__":
    asyncio.run(main())
```

::: tip
Install the Code Interpreter SDK separately: `pip install opensandbox-code-interpreter`
:::

## 4. Try the CLI

```bash
pip install opensandbox-cli

osb config init
osb config set connection.domain localhost:8080
osb config set connection.protocol http
osb sandbox create --image python:3.12 --timeout 30m -o json
osb command run <sandbox-id> -o raw -- python -c "print(1 + 1)"
```

## Next Steps

- [Installation](/getting-started/installation) — Detailed installation for all SDKs and runtimes
- [Configuration](/getting-started/configuration) — Server configuration reference
- [Architecture](/architecture/) — How OpenSandbox works under the hood
- [Guides](/guides/credential-vault) — Feature guides for Credential Vault, Secure Containers, and more
- [Examples](/examples/) — Real-world usage examples
