# OpenSandbox Code Interpreter SDK for Python

English | [中文](README_zh.md)

A Python SDK for executing code in secure, isolated sandboxes. It provides a high-level API for running Python, Java,
Go, TypeScript, and other languages safely, with support for code execution contexts.

## Prerequisites

This SDK requires a Docker image containing the Code Interpreter runtime environment. You must use the
`opensandbox/code-interpreter` image (or a derivative) which includes pre-installed runtimes for Python, Java, Go,
Node.js, etc.

For detailed information about supported languages and versions, refer to the
[Environment Documentation](../../../sandboxes/code-interpreter/README.md).

## Installation

### pip

```bash
pip install opensandbox-code-interpreter
```

### uv

```bash
uv add opensandbox-code-interpreter
```

## Quick Start

The following example demonstrates how to create a sandbox with a specific runtime configuration and execute a simple
script.

```python
import asyncio
from datetime import timedelta

from code_interpreter import CodeInterpreter, SupportedLanguage
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig


async def main() -> None:
    # 1. Configure connection
    config = ConnectionConfig(
        domain="api.opensandbox.io",
        api_key="your-api-key",
        request_timeout=timedelta(seconds=60),
    )

    # 2. Create a Sandbox with the code-interpreter image + runtime versions
    sandbox = await Sandbox.create(
        "opensandbox/code-interpreter:v1.0.2",
        connection_config=config,
        entrypoint=["/opt/opensandbox/code-interpreter.sh"],
        env={
            "PYTHON_VERSION": "3.11",
            "JAVA_VERSION": "17",
            "NODE_VERSION": "20",
            "GO_VERSION": "1.24",
        },
    )

    # 3. Use async context manager to ensure local resources are cleaned up
    async with sandbox:
        # 4. Create CodeInterpreter wrapper
        interpreter = await CodeInterpreter.create(sandbox=sandbox)

        # 5. Create an execution context (Python)
        context = await interpreter.codes.create_context(SupportedLanguage.PYTHON)

        # 6. Run code
        result = await interpreter.codes.run(
            "import sys\nprint(sys.version)\nresult = 2 + 2\nresult",
            context=context,
        )

        # Alternatively, you can pass a language directly (recommended: SupportedLanguage.*).
        # This uses the default context for that language (state can persist across runs).
        # result = await interpreter.codes.run("print('hi')", language=SupportedLanguage.PYTHON)

        # 7. Print output
        if result.result:
            print(result.result[0].text)

        # 8. Cleanup remote instance (optional but recommended)
        await sandbox.kill()


if __name__ == "__main__":
    asyncio.run(main())
```

### Synchronous Quick Start

If you prefer a synchronous API, use `SandboxSync` + `CodeInterpreterSync`:

```python
from datetime import timedelta

import httpx
from code_interpreter import CodeInterpreterSync
from opensandbox import SandboxSync
from opensandbox.config import ConnectionConfigSync

config = ConnectionConfigSync(
    domain="api.opensandbox.io",
    api_key="your-api-key",
    request_timeout=timedelta(seconds=60),
    transport=httpx.HTTPTransport(limits=httpx.Limits(max_connections=20)),
)

sandbox = SandboxSync.create(
    "opensandbox/code-interpreter:v1.0.2",
    connection_config=config,
    entrypoint=["/opt/opensandbox/code-interpreter.sh"],
    env={"PYTHON_VERSION": "3.11"},
)
with sandbox:
    interpreter = CodeInterpreterSync.create(sandbox=sandbox)
    result = interpreter.codes.run("result = 2 + 2\nresult")
    if result.result:
        print(result.result[0].text)
    sandbox.kill()
```

### Installing Python packages at runtime

You can install packages directly via `sandbox.commands.run(...)`:

```python
execution = await sandbox.commands.run("pip install pandas numpy")
```

## Runtime Configuration

### Docker Image

The Code Interpreter SDK relies on a specialized environment. Ensure your sandbox provider has the
`opensandbox/code-interpreter` image available.

### Language Version Selection

You can specify the desired version of a programming language by setting the corresponding environment variable when
creating the `Sandbox`.

| Language | Environment Variable | Example Value | Default (if unset) |
| -------- | -------------------- | ------------- | ------------------ |
| Python   | `PYTHON_VERSION`     | `3.11`        | Image default      |
| Java     | `JAVA_VERSION`       | `17`          | Image default      |
| Node.js  | `NODE_VERSION`       | `20`          | Image default      |
| Go       | `GO_VERSION`         | `1.24`        | Image default      |

## Usage Examples

### 0. Run with `language` (default language context)

You can pass `language` directly (recommended: `SupportedLanguage.*`) and skip `create_context`.
When `context.id` is omitted, **execd will create/reuse a default session for that language**, so
state can persist across runs:

```python
from code_interpreter import SupportedLanguage

execution = await interpreter.codes.run(
    "result = 2 + 2\nresult",
    language=SupportedLanguage.PYTHON,
)
assert execution.result and execution.result[0].text == "4"
```

State persistence example (default Python context):

```python
from code_interpreter import SupportedLanguage

await interpreter.codes.run("x = 42", language=SupportedLanguage.PYTHON)
execution = await interpreter.codes.run("result = x\nresult", language=SupportedLanguage.PYTHON)
assert execution.result and execution.result[0].text == "42"
```

### 1. Java Code Execution

```python
from code_interpreter import SupportedLanguage

ctx = await interpreter.codes.create_context(SupportedLanguage.JAVA)
execution = await interpreter.codes.run(
    (
        'System.out.println("Calculating sum...");\n'
        + "int a = 10;\n"
        + "int b = 20;\n"
        + "int sum = a + b;\n"
        + 'System.out.println("Sum: " + sum);\n'
        + "sum"
    ),
    context=ctx,
)

print(execution.id)
for msg in execution.logs.stdout:
    print(msg.text)
```

### 2. Python with State Persistence

Variables defined in one execution are available in subsequent executions within the same context.

```python
from code_interpreter import SupportedLanguage

ctx = await interpreter.codes.create_context(SupportedLanguage.PYTHON)

await interpreter.codes.run(
    "users = ['Alice', 'Bob', 'Charlie']\nprint(len(users))",
    context=ctx,
)

result = await interpreter.codes.run(
    "users.append('Dave')\nprint(users)\nresult = users\nresult",
    context=ctx,
)
```

### 3. Streaming Output Handling

Handle stdout/stderr and execution events in real-time.

```python
from opensandbox.models.execd import ExecutionHandlers
from code_interpreter import SupportedLanguage

async def on_stdout(msg):
    print("STDOUT:", msg.text)

async def on_stderr(msg):
    print("STDERR:", msg.text)

handlers = ExecutionHandlers(on_stdout=on_stdout, on_stderr=on_stderr)

ctx = await interpreter.codes.create_context(SupportedLanguage.PYTHON)
await interpreter.codes.run(
    "import time\nfor i in range(5):\n    print(i)\n    time.sleep(0.5)",
    context=ctx,
    handlers=handlers,
)
```

### 4. Multi-Language Context Isolation

Different languages run in isolated environments.

```python
from code_interpreter import SupportedLanguage

py_ctx = await interpreter.codes.create_context(SupportedLanguage.PYTHON)
go_ctx = await interpreter.codes.create_context(SupportedLanguage.GO)

await interpreter.codes.run("print('Running in Python')", context=py_ctx)
await interpreter.codes.run(
    "package main\nfunc main() { println(\"Running in Go\") }",
    context=go_ctx,
)
```

## Notes

- **Lifecycle**: `CodeInterpreter` wraps an existing `Sandbox` instance and reuses its connection configuration.
- **Asyncio/event loop**: avoid sharing long-lived clients across multiple event loops (e.g. pytest-asyncio defaults).
