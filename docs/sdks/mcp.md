---
title: MCP Sandbox Server
description: OpenSandbox MCP Server exposes sandbox operations as MCP tools for Claude Code, Cursor, and other MCP-capable clients.
---

# OpenSandbox MCP Sandbox Server

## Overview

OpenSandbox MCP Server exposes the OpenSandbox Python SDK as MCP tools for
Claude Code, Cursor, and other MCP-capable clients. It provides focused
sandbox lifecycle management, command execution, and text file operations.

## Installation & Startup

### Source

```bash
cd sdks/mcp/sandbox/python
uv sync
uv run opensandbox-mcp
```

### Package

```bash
pip install opensandbox-mcp
opensandbox-mcp
```

### Configuration

Environment variables:

- `OPEN_SANDBOX_API_KEY`
- `OPEN_SANDBOX_DOMAIN`

CLI overrides:

```bash
opensandbox-mcp --api-key ... --domain ... --protocol https
```

Config fields:

- `api_key`: OpenSandbox API key for authentication.
- `domain`: OpenSandbox API domain, for example `api.opensandbox.io`.
- `protocol`: `http` or `https` for API requests.
- `request_timeout_seconds`: HTTP request timeout in seconds.
- `transport`: `stdio` by default, or `streamable-http`.

### Streamable HTTP

```bash
opensandbox-mcp \
  --transport streamable-http
```

## Integrations

### Claude Code stdio

```bash
claude mcp add opensandbox-sandbox --transport stdio -- \
  opensandbox-mcp --api-key "$OPEN_SANDBOX_API_KEY" --domain "$OPEN_SANDBOX_DOMAIN"
```

### Claude Code http

```bash
claude mcp add opensandbox-sandbox --transport http http://localhost:8000/mcp
```

### Cursor stdio

```json
{
  "mcpServers": {
    "opensandbox-sandbox": {
      "command": "opensandbox-mcp",
      "args": [
        "--api-key",
        "${OPEN_SANDBOX_API_KEY}",
        "--domain",
        "${OPEN_SANDBOX_DOMAIN}"
      ]
    }
  }
}
```

### Cursor http

```json
{
  "mcpServers": {
    "opensandbox-sandbox": {
      "url": "http://localhost:8000/mcp"
    }
  }
}
```

## Tools

::: info
- All tools operate on a `sandbox_id` returned by `sandbox_create` or `sandbox_connect`.
- `file_read`/`file_write` are text-only; use `encoding` and `range_header` for large files.
:::

### Sandbox

- `sandbox_create`: create a new sandbox and register it locally
- `sandbox_connect`: attach to an existing sandbox and register it locally
- `sandbox_kill`: terminate a sandbox by ID
- `sandbox_get_info`: fetch sandbox info by ID
- `sandbox_list`: list sandboxes with optional `filter` object
- `sandbox_renew`: extend sandbox expiration
- `sandbox_healthcheck`: check if sandbox is healthy
- `sandbox_get_metrics`: get resource metrics
- `sandbox_get_endpoint`: get network endpoint for a port

### Command Execution

- `command_run`: run a command inside a sandbox
- `command_interrupt`: interrupt a running command

### Filesystem

- `file_read`: read a text file
- `file_write`: write a text file
- `file_delete`: delete files
- `file_search`: search for files by glob
- `file_create_directories`: create directories
- `file_delete_directories`: delete directories
- `file_move`: move/rename files or directories
- `file_replace_contents`: replace file content

## Minimal Workflow

1. `sandbox_create` -> keep the `sandbox_id`.
2. `file_write` code or assets into the sandbox.
3. `command_run` to execute, install dependencies, or start a service.
4. `sandbox_get_endpoint` if you expose a port.
5. `sandbox_kill` when finished.

## Usage Examples

Here are some examples of what you can ask an LLM to do:

- "Create a Python sandbox and run a quick health command."
- "Write a Python script into the sandbox and run it."
- "Download a GitHub repo, install dependencies, and run its tests."
- "Generate a CSV file with fake sales data and run a simple summary script."
- "Start a tiny web server on port 8000 and return the public URL."
- "Build a minimal REST API (hello + health) and expose it on port 8000."
- "Create a tar.gz of /app and report the file size."
- "Build a simple Snake game and return the web endpoint where it can be accessed."
