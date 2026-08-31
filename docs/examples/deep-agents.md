---
title: Deep Agents
description: Run a Deep Agent with its file and shell tools executing inside an OpenSandbox sandbox.
---

# Deep Agents + OpenSandbox Example

Run a [Deep Agent](https://github.com/langchain-ai/deepagents) whose file and shell tools
execute inside an OpenSandbox sandbox. The
[`langchain-sandbox-opensandbox`](https://pypi.org/project/langchain-sandbox-opensandbox/)
backend adapts the OpenSandbox Python SDK to the Deep Agents `BaseSandbox` interface, so every
`ls` / `read_file` / `write_file` / `glob` / `grep` / command the agent performs is sandboxed.

The backend passes the full `langchain-tests` `SandboxIntegrationTests` conformance suite
(86/86) against a live server.

## Start OpenSandbox server [local]

Start a local OpenSandbox server, logs will be visible in the terminal:

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Run the example

```shell
# Install Deep Agents + the OpenSandbox backend
uv pip install deepagents langchain-sandbox-opensandbox

# Run the example (requires SANDBOX_DOMAIN / ANTHROPIC_API_KEY)
uv run python examples/deep-agents/main.py
```

The agent writes a Python script into the sandbox, runs it, and reports the output — all file
and shell operations happen inside the sandbox rather than on the host. The sandbox is destroyed
on exit (terminated and local resources closed).

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address (host and optional port) |
| `SANDBOX_PROTOCOL` | `http` | Protocol used to reach the server (`http` or `https`) |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if your server requires authentication |
| `ANTHROPIC_API_KEY` | _(required)_ | Anthropic API key for the default Deep Agents model |

## References

- [Deep Agents](https://github.com/langchain-ai/deepagents) - Agent framework
- [langchain-sandbox-opensandbox](https://pypi.org/project/langchain-sandbox-opensandbox/) - OpenSandbox backend for Deep Agents
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/deep-agents)
