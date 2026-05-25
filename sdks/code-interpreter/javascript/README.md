# Alibaba Code Interpreter SDK for JavaScript/TypeScript

English | [中文](README_zh.md)

A TypeScript/JavaScript SDK for executing code in secure, isolated sandboxes. It provides a high-level API for running Python, Java, Go, TypeScript, and other languages safely, with support for code execution contexts.

## Prerequisites

This SDK requires a Docker image containing the Code Interpreter runtime environment. You must use the `opensandbox/code-interpreter` image (or a derivative) which includes pre-installed runtimes for Python, Java, Go, Node.js, etc.

For detailed information about supported languages and versions, please refer to the [Environment Documentation](../../../sandboxes/code-interpreter/README.md).

## Installation

### npm

```bash
npm install @alibaba-group/opensandbox-code-interpreter
```

### pnpm

```bash
pnpm add @alibaba-group/opensandbox-code-interpreter
```

### yarn

```bash
yarn add @alibaba-group/opensandbox-code-interpreter
```

## Quick Start

The following example demonstrates how to create a sandbox with a specific runtime configuration and execute a simple script.

> **Note**: Before running this example, ensure the OpenSandbox service is running. See the root [README.md](../../../README.md) for startup instructions.

```ts
import { ConnectionConfig, Sandbox } from "@alibaba-group/opensandbox";
import { CodeInterpreter, SupportedLanguages } from "@alibaba-group/opensandbox-code-interpreter";

// 1. Configure connection
const config = new ConnectionConfig({
  domain: "api.opensandbox.io",
  apiKey: "your-api-key",
});

// 2. Create a Sandbox with the code-interpreter image + runtime versions
const sandbox = await Sandbox.create({
  connectionConfig: config,
  image: "opensandbox/code-interpreter:v1.0.2",
  entrypoint: ["/opt/opensandbox/code-interpreter.sh"],
  env: {
    PYTHON_VERSION: "3.11",
    JAVA_VERSION: "17",
    NODE_VERSION: "20",
    GO_VERSION: "1.24",
  },
  timeoutSeconds: 15 * 60,
});

// 3. Create CodeInterpreter wrapper
const ci = await CodeInterpreter.create(sandbox);

// 4. Create an execution context (Python)
const ctx = await ci.codes.createContext(SupportedLanguages.PYTHON);

// 5. Run code
const result = await ci.codes.run("import sys\nprint(sys.version)\nresult = 2 + 2\nresult", {
  context: ctx,
});

// 6. Print output
console.log(result.result[0]?.text);

// 7. Cleanup remote instance (optional but recommended)
await sandbox.kill();
await sandbox.close();
```

## Runtime Configuration

### Docker Image

The Code Interpreter SDK relies on a specialized environment. Ensure your sandbox provider has the `opensandbox/code-interpreter` image available.

### Language Version Selection

You can specify the desired version of a programming language by setting the corresponding environment variable when creating the `Sandbox`.

| Language | Environment Variable | Example Value | Default (if unset) |
| --- | --- | --- | --- |
| Python | `PYTHON_VERSION` | `3.11` | Image default |
| Java | `JAVA_VERSION` | `17` | Image default |
| Node.js | `NODE_VERSION` | `20` | Image default |
| Go | `GO_VERSION` | `1.24` | Image default |

```ts
const sandbox = await Sandbox.create({
  connectionConfig: config,
  image: "opensandbox/code-interpreter:v1.0.2",
  entrypoint: ["/opt/opensandbox/code-interpreter.sh"],
  env: {
    JAVA_VERSION: "17",
    GO_VERSION: "1.24",
  },
});
```

## Usage Examples

### 0. Run with `language` (default language context)

If you don't need to manage explicit context IDs, you can run code by specifying only `language`.
When `context.id` is omitted, execd can create/reuse a default session for that language, so state can persist across runs.

```ts
import { SupportedLanguages } from "@alibaba-group/opensandbox-code-interpreter";

await ci.codes.run("x = 42", { language: SupportedLanguages.PYTHON });
const execution = await ci.codes.run("result = x\nresult", { language: SupportedLanguages.PYTHON });
console.log(execution.result[0]?.text); // "42"
```

### 0.1 Context management (list/get/delete)

You can manage contexts explicitly (aligned with Python/Kotlin SDKs):

```ts
const ctx = await ci.codes.createContext(SupportedLanguages.PYTHON);

const same = await ci.codes.getContext(ctx.id!);
console.log(same.id, same.language);

const all = await ci.codes.listContexts();
const pyOnly = await ci.codes.listContexts(SupportedLanguages.PYTHON);

await ci.codes.deleteContext(ctx.id!);
await ci.codes.deleteContexts(SupportedLanguages.PYTHON); // bulk cleanup
```

### 1. Java Code Execution

```ts
import { SupportedLanguages } from "@alibaba-group/opensandbox-code-interpreter";

const javaCtx = await ci.codes.createContext(SupportedLanguages.JAVA);
const execution = await ci.codes.run(
  [
    'System.out.println("Calculating sum...");',
    "int a = 10;",
    "int b = 20;",
    "int sum = a + b;",
    'System.out.println("Sum: " + sum);',
    "sum",
  ].join("\n"),
  { context: javaCtx },
);
console.log(execution.logs.stdout.map((m) => m.text));
```

### 2. Streaming Output Handling

Handle stdout/stderr and execution events in real-time.

```ts
import type { ExecutionHandlers } from "@alibaba-group/opensandbox";
import { SupportedLanguages } from "@alibaba-group/opensandbox-code-interpreter";

const handlers: ExecutionHandlers = {
  onStdout: (m) => console.log("STDOUT:", m.text),
  onStderr: (m) => console.error("STDERR:", m.text),
  onResult: (r) => console.log("RESULT:", r.text),
};

const pyCtx = await ci.codes.createContext(SupportedLanguages.PYTHON);
await ci.codes.run("import time\nfor i in range(5):\n    print(i)\n    time.sleep(0.2)", {
  context: pyCtx,
  handlers,
});
```

## Notes

- **Lifecycle**: `CodeInterpreter` wraps an existing `Sandbox` instance and reuses its connection configuration. Each sandbox instance clones the transport via `ConnectionConfig.withTransportIfMissing()`, so call `sandbox.close()` when you are finished to release the Node.js keep-alive agent and avoid leak.
- **Default context**: `codes.run(..., { language })` uses a language default context (state can persist across runs).

