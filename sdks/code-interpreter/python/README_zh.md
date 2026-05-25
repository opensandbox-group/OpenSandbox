# OpenSandbox Code Interpreter SDK for Python

中文 | [English](README.md)

一个用于在安全、隔离的沙箱环境中执行代码的 Python SDK。该 SDK 提供了高级 API，支持安全地运行 Python、Java、Go、TypeScript
等语言，并具备“代码执行上下文（Context）”能力。

## 前置要求

本 SDK 需要配合包含 Code Interpreter 运行时环境的特定 Docker 镜像使用。请务必使用 `opensandbox/code-interpreter` 镜像（或其衍生镜像），其中预装了 Python、Java、Go、Node.js 等语言的运行环境。

关于支持的语言与具体版本信息，请参考 [环境文档](../../../sandboxes/code-interpreter/README_zh.md)。

## 安装指南

### pip

```bash
pip install opensandbox-code-interpreter
```

### uv

```bash
uv add opensandbox-code-interpreter
```

## 快速开始

以下示例展示了如何创建带指定运行时配置的 Sandbox，并执行一段简单脚本。

```python
import asyncio
from datetime import timedelta

from code_interpreter import CodeInterpreter, SupportedLanguage
from opensandbox import Sandbox
from opensandbox.config import ConnectionConfig


async def main() -> None:
    # 1. 配置连接信息
    config = ConnectionConfig(
        domain="api.opensandbox.io",
        api_key="your-api-key",
        request_timeout=timedelta(seconds=60),
    )

    # 2. 创建 Sandbox（必须使用 code-interpreter 镜像），并指定语言版本
    sandbox = await Sandbox.create(
        "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2",
        connection_config=config,
        entrypoint=["/opt/opensandbox/code-interpreter.sh"],
        env={
            "PYTHON_VERSION": "3.11",
            "JAVA_VERSION": "17",
            "NODE_VERSION": "20",
            "GO_VERSION": "1.24",
        },
    )

    # 3. 使用异步上下文管理器，确保本地资源正确清理
    async with sandbox:
        # 4. 创建 CodeInterpreter 包装器
        interpreter = await CodeInterpreter.create(sandbox=sandbox)

        # 5. 创建执行上下文（Python）
        context = await interpreter.codes.create_context(SupportedLanguage.PYTHON)

        # 6. 运行代码
        result = await interpreter.codes.run(
            "import sys\nprint(sys.version)\nresult = 2 + 2\nresult",
            context=context,
        )

        # 或者：直接传入 language（推荐使用 SupportedLanguage.*），使用该语言默认上下文执行（可跨次保持状态）
        # result = await interpreter.codes.run("print('hi')", language=SupportedLanguage.PYTHON)

        # 7. 打印输出
        if result.result:
            print(result.result[0].text)

        # 8. 清理远程实例（可选，但推荐）
        await sandbox.kill()


if __name__ == "__main__":
    asyncio.run(main())
```

### 同步版本快速开始

如果你更偏好同步 API，可以使用 `SandboxSync` + `CodeInterpreterSync`：

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
    "sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.0.2",
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

### 运行时安装 Python 依赖

可以直接通过 `sandbox.commands.run(...)` 安装依赖：

```python
execution = await sandbox.commands.run("pip install pandas numpy")
```

## 运行时配置

### Docker 镜像

Code Interpreter SDK 依赖于特定的运行环境。请确保你的沙箱服务提供商支持 `opensandbox/code-interpreter` 镜像。

### 语言版本选择

你可以在创建 `Sandbox` 时通过环境变量指定所需的编程语言版本。

| 语言    | 环境变量         | 示例值 | 默认值（若不设置） |
| ------- | ---------------- | ------ | ------------------ |
| Python  | `PYTHON_VERSION` | `3.11` | 镜像默认值         |
| Java    | `JAVA_VERSION`   | `17`   | 镜像默认值         |
| Node.js | `NODE_VERSION`   | `20`   | 镜像默认值         |
| Go      | `GO_VERSION`     | `1.24` | 镜像默认值         |

## 核心功能示例

### 0. 直接传 `language`（使用该语言默认上下文）

可以直接传入 `language`（推荐：`SupportedLanguage.*`），跳过 `create_context`。
当 `context.id` 省略时，**execd 会为该语言创建/复用默认 session**，因此状态可以跨次执行保持：

```python
from code_interpreter import SupportedLanguage

execution = await interpreter.codes.run(
    "result = 2 + 2\nresult",
    language=SupportedLanguage.PYTHON,
)
assert execution.result and execution.result[0].text == "4"
```

状态持久化示例（Python 默认上下文）：

```python
from code_interpreter import SupportedLanguage

await interpreter.codes.run("x = 42", language=SupportedLanguage.PYTHON)
execution = await interpreter.codes.run("result = x\nresult", language=SupportedLanguage.PYTHON)
assert execution.result and execution.result[0].text == "42"
```

### 1. Java 代码执行

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

### 2. Python 持久化状态

在同一个上下文中，变量状态可以跨次执行保持。

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

### 3. 流式输出处理

实时处理 stdout/stderr 等事件。

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

### 4. 多语言上下文隔离

不同语言在隔离的环境中运行。

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

## 说明

- **生命周期**：`CodeInterpreter` 基于既有的 `Sandbox` 实例进行包装，并复用其连接配置。
- **Asyncio/event loop**：避免在多个 event loop 间共享长生命周期的 client/transport（例如 pytest-asyncio 默认行为）。
