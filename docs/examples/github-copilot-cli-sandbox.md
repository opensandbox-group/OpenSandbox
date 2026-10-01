---
title: GitHub Copilot CLI
description: Run GitHub Copilot CLI inside a local OpenSandbox container.
---

# GitHub Copilot CLI Example

Run the official GitHub Copilot CLI inside an OpenSandbox container. The
example downloads the official CLI release and executes a prompt
non-interactively with `copilot -sp`.

The example follows the same local workflow as the OpenCode example. The
Python client only uses the OpenSandbox SDK; the local OpenSandbox Server
creates and manages the container through its Docker runtime.

The sandbox uses the Docker Hub image
`opensandbox/code-interpreter:v1.1.0`; no Alibaba Cloud container registry is
used.

## Start OpenSandbox server [local]

Pre-pull the code-interpreter image:

```shell
docker pull opensandbox/code-interpreter:v1.1.0
```

Install the dependencies and initialize the local Docker runtime:

```shell
uv sync
uv run opensandbox-server init-config ~/.sandbox.toml --example docker
OPENSANDBOX_INSECURE_SERVER=YES uv run opensandbox-server
```

`OPENSANDBOX_INSECURE_SERVER=YES` explicitly acknowledges that this local
server has no API key. Keep it bound to localhost. For a shared deployment,
configure `server.api_key` instead.

The project currently pins `opensandbox-server==0.2.3`. The upstream
`1.1.0` server wheel is known to be missing its generated FastPath gRPC
modules and fails during startup.

## Create and Access the GitHub Copilot Sandbox

Create a fine-grained GitHub personal access token with the
**Copilot Requests** permission. Store it in the ignored local `.env` file:

```shell
GH_TOKEN=github_pat_replace_me
```

Run the example from the repository root:

```shell
uv run python examples/github-copilot-cli-sandbox/main.py
```

The script creates a Docker-backed sandbox, installs GitHub Copilot CLI with
the official installation script, runs `copilot -sp` in an isolated working
directory, and destroys the container afterward.

The Microsoft npm proxy does not currently provide `@github/copilot`, so this
example uses the official installer, which downloads and verifies a GitHub
Release artifact.

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `GH_TOKEN` | _(required)_ | Fine-grained PAT with **Copilot Requests** permission |
| `GITHUB_TOKEN` | _(optional)_ | Used when `GH_TOKEN` is not set |
| `SANDBOX_DOMAIN` | `localhost:8080` | Local sandbox service address |
| `SANDBOX_API_KEY` | _(optional for local)_ | API key if configured |
| `SANDBOX_IMAGE` | `opensandbox/code-interpreter:v1.1.0` | Docker Hub sandbox image |
| `COPILOT_PROMPT` | Arithmetic example | Prompt passed to `copilot -sp` |
| `COPILOT_VERSION` | Latest stable version | Optional official CLI release to install |

::: warning
Never commit a GitHub token. If a token is exposed in terminal output,
documentation, source code, or chat, revoke it immediately and create a new
one.
:::

## References

- [GitHub Copilot CLI](https://github.com/github/copilot-cli)
- [GitHub Copilot CLI quickstart](https://docs.github.com/en/copilot/get-started/cli-quickstart)
- [Source code](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/github-copilot)
