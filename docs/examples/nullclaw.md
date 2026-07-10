---
title: NullClaw
description: Launch a Nullclaw Gateway inside an OpenSandbox instance and expose its HTTP endpoint.
---

# Nullclaw Gateway Example

Launch a [Nullclaw](https://github.com/nullclaw/nullclaw) Gateway inside an OpenSandbox instance and expose its HTTP endpoint. The script polls the gateway health check until it returns HTTP 200, then prints the reachable endpoint.

## Start OpenSandbox server [local]

You can find the latest Nullclaw container image [here](https://github.com/nullclaw/nullclaw/pkgs/container/nullclaw).

::: info Docker runtime requirement
The server uses `runtime.type = "docker"` by default, so it **must** be able to reach a running Docker daemon.

- **Docker Desktop**: ensure Docker Desktop is running, then verify with `docker version`.
- **Colima (macOS)**: start it first (`colima start`) and export the socket before starting the server:

```shell
export DOCKER_HOST="unix://${HOME}/.colima/default/docker.sock"
```
:::

Pre-pull the Nullclaw image:

```shell
docker pull ghcr.io/nullclaw/nullclaw:latest
```

Start the OpenSandbox server (logs will stay in the terminal):

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

## Create and Access the Nullclaw Sandbox

This example is hard-coded for a quick start:
- OpenSandbox server: `http://localhost:8080`
- Image: `ghcr.io/nullclaw/nullclaw:latest`
- Gateway port: `3000`
- Timeout: `3600s`

Install dependencies from the project root:

```shell
uv pip install opensandbox requests
```

Run the example:

```shell
uv run python examples/nullclaw/main.py
```

You should see output similar to:

```text
Creating nullclaw sandbox with image=ghcr.io/nullclaw/nullclaw:latest on OpenSandbox server http://localhost:8080...
[check] sandbox ready after 0.3s
Nullclaw gateway started. Please refer to 127.0.0.1:56234
```

The endpoint printed at the end (e.g., `127.0.0.1:56234`) is the Nullclaw Gateway address exposed from the sandbox.

::: tip
By default, Nullclaw requires pairing before authenticated endpoints (for example, `/webhook`) can be used. The `/health` endpoint remains publicly accessible.
:::

## References

- [Nullclaw](https://github.com/nullclaw/nullclaw) -- Minimal AI assistant runtime (678 KB static Zig binary)
- [Nullclaw Documentation](https://nullclaw.github.io) -- Full documentation
- [OpenSandbox Python SDK](https://pypi.org/project/opensandbox/)
- [Source code on GitHub](https://github.com/opensandbox-group/OpenSandbox/tree/main/examples/nullclaw)
