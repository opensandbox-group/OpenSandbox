---
title: Installation
description: Install OpenSandbox server, SDKs, CLI, and MCP server across all supported languages and platforms.
---

# Installation

## Server

The OpenSandbox server is a FastAPI-based service that manages sandbox lifecycles. It supports Docker and Kubernetes runtimes.

```bash
# Install from PyPI
uv pip install opensandbox-server

# Or with pip
pip install opensandbox-server
```

**Requirements:**
- Python 3.10+
- Docker Engine 20.10+ (Docker runtime) or Kubernetes 1.21.1+ (Kubernetes runtime)
- Linux, macOS, or Windows with WSL2

See [Configuration](/getting-started/configuration) for server setup.

## SDKs

### Sandbox SDKs

The core SDKs for sandbox lifecycle management, command execution, and file operations.

::: code-group

```bash [Python]
pip install opensandbox
```

```bash [JavaScript/TypeScript]
npm install @alibaba-group/opensandbox
```

```kotlin [Kotlin/Java (Gradle)]
dependencies {
    implementation(platform("com.alibaba.opensandbox:sandbox-bom:{latest_version}"))
    implementation("com.alibaba.opensandbox:sandbox")
}
```

```xml [Kotlin/Java (Maven)]
<dependency>
    <groupId>com.alibaba.opensandbox</groupId>
    <artifactId>sandbox</artifactId>
    <version>{latest_version}</version>
</dependency>
```

```bash [Go]
go get github.com/alibaba/OpenSandbox/sdks/sandbox/go
```

```bash [C#/.NET]
dotnet add package Alibaba.OpenSandbox
```

:::

For detailed SDK usage, see the [SDK documentation](/sdks/).

### Code Interpreter SDKs

Higher-level SDKs for multi-language code execution inside sandboxes.

::: code-group

```bash [Python]
pip install opensandbox-code-interpreter
```

```bash [JavaScript/TypeScript]
npm install @alibaba-group/opensandbox-code-interpreter
```

```kotlin [Kotlin/Java (Gradle)]
dependencies {
    implementation(platform("com.alibaba.opensandbox:sandbox-bom:{latest_version}"))
    implementation("com.alibaba.opensandbox:sandbox")
    implementation("com.alibaba.opensandbox:code-interpreter")
}
```

```bash [C#/.NET]
dotnet add package Alibaba.OpenSandbox.CodeInterpreter
```

:::

## CLI

The `osb` CLI provides terminal-based sandbox management.

```bash
pip install opensandbox-cli
# or
uv tool install opensandbox-cli
```

See the [CLI reference](/cli/) for the full command set.

## MCP Server

The MCP server exposes sandbox operations to MCP-capable clients like Claude Code and Cursor.

```bash
pip install opensandbox-mcp
opensandbox-mcp --domain localhost:8080 --protocol http
```

Minimal stdio config for MCP clients:

```json
{
  "mcpServers": {
    "opensandbox": {
      "command": "opensandbox-mcp",
      "args": ["--domain", "localhost:8080", "--protocol", "http"]
    }
  }
}
```

See the [MCP documentation](/sdks/mcp) for client-specific setup.
