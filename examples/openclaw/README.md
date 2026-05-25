# OpenClaw Gateway Example

Launch an [OpenClaw](https://github.com/openclaw/openclaw) Gateway inside an OpenSandbox instance and expose its HTTP endpoint. The script polls the gateway until it returns HTTP 200, then prints the reachable endpoint.

## Quick Start

```shell
# Install dependencies
uv pip install opensandbox requests

# Run with default settings
uv run python examples/openclaw/main.py
```

## Configuration Options

The example supports various environment variables for customization:

| Variable | Default | Description |
|----------|---------|-------------|
| `OPENCLAW_SERVER` | `http://localhost:8080` | OpenSandbox server address |
| `OPENCLAW_TOKEN` | `dummy-token-for-sandbox` | Gateway authentication token |
| `OPENCLAW_IMAGE` | `ghcr.io/openclaw/openclaw:latest` | Container image |
| `OPENCLAW_TIMEOUT` | `3600` | Sandbox timeout in seconds |

## Network Policy

By default, the sandbox denies all network access except `pypi.org` (for package installation). You can customize this in `main.py`:

```python
network_policy=NetworkPolicy(
    defaultAction="deny",
    egress=[
        NetworkRule(action="allow", target="pypi.org"),
        NetworkRule(action="allow", target="pypi.python.org"),
        # Add more allowed targets
    ],
)
```

## Environment Variables for OpenClaw

Pass environment variables to the OpenClaw Gateway inside the sandbox:

```python
env={
    "OPENCLAW_GATEWAY_TOKEN": token,
    "OPENCLAW_MODEL": "claude-sonnet-4-20250514",
    # Add more env vars as needed
},
```

## Start OpenSandbox server [local]

You can find the latest OpenClaw container image [here](https://github.com/openclaw/openclaw/pkgs/container/openclaw).

### Notes (Docker runtime requirement)

The server uses `runtime.type = "docker"` by default, so it **must** be able to reach a running Docker daemon.

- **Docker Desktop**: ensure Docker Desktop is running, then verify with `docker version`.
- **Colima (macOS)**: start it first (`colima start`) and export the socket before starting the server:

```shell
export DOCKER_HOST="unix://${HOME}/.colima/default/docker.sock"
```

Pre-pull the OpenClaw image:

```shell
docker pull ghcr.io/openclaw/openclaw:latest
```

Start the OpenSandbox server (logs will stay in the terminal):

```shell
uv pip install opensandbox-server
opensandbox-server init-config ~/.sandbox.toml --example docker
opensandbox-server
```

If you see errors like `FileNotFoundError: [Errno 2] No such file or directory` from `docker/transport/unixconn.py`, it usually means the Docker unix socket is missing or Docker is not running.

## Create and Access the OpenClaw Sandbox

This example is hard-coded for a quick start:
- OpenSandbox server: `http://localhost:8080`
- Image: `ghcr.io/openclaw/openclaw:latest`
- Gateway port: `18789`
- Timeout: `3600s`
- Token: `OPENCLAW_GATEWAY_TOKEN` (default: `dummy-token-for-sandbox`)

Install dependencies from the project root:

```shell
uv pip install opensandbox requests
```

Run the example (set a real token if you need authenticated access):

```shell
export OPENCLAW_GATEWAY_TOKEN="$(openssl rand -hex 32)"
uv run python examples/openclaw/main.py
```

You should see output similar to:

```text
Creating openclaw sandbox with image=ghcr.io/openclaw/openclaw:latest on OpenSandbox server http://localhost:8080...
[check] sandbox ready after 7.1s
Openclaw started finished. Please refer to 127.0.0.1:56123
```

The endpoint printed at the end (e.g., `127.0.0.1:56123`) is the OpenClaw Gateway address exposed from the sandbox.

## Advanced: Custom Gateway Port

To use a custom port, modify the `entrypoint` in `main.py`:

```python
entrypoint=["node dist/index.js gateway --bind=lan --port 19999 --allow-unconfigured --verbose"],
```

Then update the port in the `get_endpoint()` call:

```python
endpoint = sandbox.get_endpoint(19999)
```

## References
- [OpenClaw](https://github.com/openclaw/openclaw)
- [OpenSandbox Python SDK](https://pypi.org/project/opensandbox/)
