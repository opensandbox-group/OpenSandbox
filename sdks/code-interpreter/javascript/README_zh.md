# Alibaba Code Interpreter JavaScript/TypeScript SDK

中文 | [English](README.md)

一个用于在安全、隔离的沙箱环境中执行代码的 TypeScript/JavaScript SDK。该 SDK 提供了高级 API，支持安全地运行 Python、Java、Go、TypeScript 等语言，并具备“代码执行上下文（Context）”能力。

## 前置要求

本 SDK 需要配合包含 Code Interpreter 运行时环境的特定 Docker 镜像使用。请务必使用 `opensandbox/code-interpreter` 镜像（或其衍生镜像），其中预装了 Python、Java、Go、Node.js 等语言的运行环境。

关于支持的语言与具体版本信息，请参考 [环境文档](../../../sandboxes/code-interpreter/README_zh.md)。

## 安装指南

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

## 快速开始

以下示例展示了如何创建带指定运行时配置的 Sandbox，并执行一段简单脚本。

> **注意**: 在运行此示例之前，请确保 OpenSandbox 服务已启动。服务启动请参考根目录的 [README_zh.md](../../../docs/README_zh.md)。

```ts
import { ConnectionConfig, Sandbox } from "@alibaba-group/opensandbox";
import { CodeInterpreter, SupportedLanguages } from "@alibaba-group/opensandbox-code-interpreter";

// 1. 配置连接信息
const config = new ConnectionConfig({
  domain: "api.opensandbox.io",
  apiKey: "your-api-key",
});

// 2. 创建 Sandbox（必须使用 code-interpreter 镜像），并指定语言版本
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

// 3. 创建 CodeInterpreter 包装器
const ci = await CodeInterpreter.create(sandbox);

// 4. 创建执行上下文（Python）
const ctx = await ci.codes.createContext(SupportedLanguages.PYTHON);

// 5. 运行代码
const result = await ci.codes.run("import sys\nprint(sys.version)\nresult = 2 + 2\nresult", {
  context: ctx,
});

// 6. 打印输出
console.log(result.result[0]?.text);

// 7. 清理远程实例（可选，但推荐）
await sandbox.kill();
await sandbox.close();
```

## 运行时配置

### Docker 镜像

Code Interpreter SDK 依赖于特定的运行环境。请确保你的沙箱服务提供商支持 `opensandbox/code-interpreter` 镜像。

### 语言版本选择

你可以在创建 `Sandbox` 时通过环境变量指定所需的编程语言版本。

| 语言 | 环境变量 | 示例值 | 默认值（若不设置） |
| --- | --- | --- | --- |
| Python | `PYTHON_VERSION` | `3.11` | 镜像默认值 |
| Java | `JAVA_VERSION` | `17` | 镜像默认值 |
| Node.js | `NODE_VERSION` | `20` | 镜像默认值 |
| Go | `GO_VERSION` | `1.24` | 镜像默认值 |

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

## 核心功能示例

### 0. 直接传 `language`（使用该语言默认上下文）

如果你不需要显式管理 context id，可以只传 `language` 来执行代码。
当 `context.id` 省略时，execd 可以为该语言创建/复用默认 session，因此状态可以跨次执行保持：

```ts
import { SupportedLanguages } from "@alibaba-group/opensandbox-code-interpreter";

await ci.codes.run("x = 42", { language: SupportedLanguages.PYTHON });
const execution = await ci.codes.run("result = x\nresult", { language: SupportedLanguages.PYTHON });
console.log(execution.result[0]?.text); // "42"
```

### 0.1 Context 管理（list/get/delete）

你也可以显式管理 context（与 Python/Kotlin SDK 对齐）：

```ts
const ctx = await ci.codes.createContext(SupportedLanguages.PYTHON);

const same = await ci.codes.getContext(ctx.id!);
console.log(same.id, same.language);

const all = await ci.codes.listContexts();
const pyOnly = await ci.codes.listContexts(SupportedLanguages.PYTHON);

await ci.codes.deleteContext(ctx.id!);
await ci.codes.deleteContexts(SupportedLanguages.PYTHON); // 批量清理
```

### 1. Java 代码执行

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

### 2. 流式输出处理

实时处理 stdout/stderr 等事件。

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

## 说明

- **生命周期**：`CodeInterpreter` 基于既有的 `Sandbox` 实例进行包装，并复用其连接配置。SDK 会通过 `ConnectionConfig.withTransportIfMissing()` 为每个实例复刻 Transport，完成交互后请调用 `sandbox.close()` 释放 Node.js 的 keep-alive agent，以避免资源泄漏。
- **默认上下文**：`codes.run(..., { language })` 会使用语言默认 context（同语言的状态可跨次执行保持）。

