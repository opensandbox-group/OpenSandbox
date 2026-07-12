---
title: LangGraph
description: Integrate LangGraph with OpenSandbox using a graph-driven control flow for agent orchestration.
---

# LangGraph Agent + OpenSandbox Example

Integrate LangGraph with OpenSandbox using a graph-driven control flow. The example uses
explicit state machine nodes to create, prepare, run, inspect, and clean up a sandbox, plus
a decision node to retry with a fallback command if the run step fails.

## Start OpenSandbox server [local]

Pre-pull the code-interpreter image (includes Python):

```shell
docker pull sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0

# use docker hub
# docker pull opensandbox/code-interpreter:v1.1.0
```

Start the local OpenSandbox server, logs will be visible in the terminal:

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Run the example

```shell
# Install OpenSandbox + LangGraph deps
uv pip install opensandbox langgraph langchain-anthropic

# Run the example (requires SANDBOX_DOMAIN / SANDBOX_API_KEY / ANTHROPIC_API_KEY)
uv run python examples/langgraph/main.py
```

The workflow writes files, executes a job, retries with a fallback command on failure (default
`python` vs `python3`), then summarizes results with Claude and cleans up the sandbox instance.

![LangGraph + OpenSandbox screenshot](../public/images/langgraph-screenshot.jpg)

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SANDBOX_DOMAIN` | `localhost:8080` | Sandbox service address |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if your server requires authentication |
| `SANDBOX_IMAGE` | `sandbox-registry.cn-zhangjiakou.cr.aliyuncs.com/opensandbox/code-interpreter:v1.1.0` | Sandbox image to use |
| `ANTHROPIC_API_KEY` | _(required if `ANTHROPIC_AUTH_TOKEN` is unset)_ | Your Anthropic API key |
| `ANTHROPIC_AUTH_TOKEN` | _(alternate)_ | Alternate Anthropic auth token (uses `Authorization` header) |
| `ANTHROPIC_BASE_URL` | _(optional)_ | Anthropic API endpoint override |
| `ANTHROPIC_MODEL` | `claude-3-5-sonnet-latest` | Model to use |

::: warning
`ANTHROPIC_API_KEY` and `ANTHROPIC_AUTH_TOKEN` should not be set together.
:::

## References

- [LangGraph](https://langchain-ai.github.io/langgraph/) - Agent workflow framework
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/langgraph)
