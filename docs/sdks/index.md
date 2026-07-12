---
title: SDKs
description: Overview of OpenSandbox multi-language SDKs for sandbox lifecycle management and code interpretation.
---

# SDKs

OpenSandbox provides SDKs in five languages covering sandbox lifecycle management, command execution, file operations, and code interpretation.

## Sandbox SDKs

The core SDKs for creating and managing sandboxes.

| Language | Package | Install |
|----------|---------|---------|
| [Python](/sdks/python) | `opensandbox` | `pip install opensandbox` |
| [JavaScript/TypeScript](/sdks/javascript) | `@alibaba-group/opensandbox` | `npm install @alibaba-group/opensandbox` |
| [Kotlin/Java](/sdks/kotlin) | `com.alibaba.opensandbox:sandbox` | Gradle/Maven |
| [Go](/sdks/go) | `github.com/alibaba/OpenSandbox/sdks/sandbox/go` | `go get` |
| [C#/.NET](/sdks/csharp) | `Alibaba.OpenSandbox` | `dotnet add package` |

## Code Interpreter SDKs

Higher-level SDKs for multi-language code execution inside sandboxes. Built on top of the sandbox SDKs.

| Language | Package | Install |
|----------|---------|---------|
| [Python](/sdks/code-interpreter/python) | `opensandbox-code-interpreter` | `pip install opensandbox-code-interpreter` |
| [JavaScript/TypeScript](/sdks/code-interpreter/javascript) | `@alibaba-group/opensandbox-code-interpreter` | `npm install @alibaba-group/opensandbox-code-interpreter` |
| [Kotlin/Java](/sdks/code-interpreter/kotlin) | `com.alibaba.opensandbox:code-interpreter` | Gradle/Maven |
| [C#/.NET](/sdks/code-interpreter/csharp) | `Alibaba.OpenSandbox.CodeInterpreter` | `dotnet add package` |

## MCP Server

The [MCP server](/sdks/mcp) exposes sandbox operations to MCP-capable clients like Claude Code and Cursor.

```bash
pip install opensandbox-mcp
```

## Common Patterns

All SDKs follow consistent patterns:

1. **Connection** — Configure server address, protocol, and API key
2. **Sandbox creation** — Specify image, entrypoint, timeout, environment, and resource limits
3. **Operations** — Execute commands, manage files, run code
4. **Cleanup** — Kill or let sandboxes expire via TTL
